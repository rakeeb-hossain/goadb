package adb

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	stderr "errors"
	"github.com/rakeeb-hossain/goadb/internal/errors"
	"github.com/rakeeb-hossain/goadb/wire"
)

// MtimeOfClose should be passed to OpenWrite to set the file modification time to the time the Close
// method is called.
var MtimeOfClose = time.Time{}

// Device communicates with a specific Android device.
// To get an instance, call Device() on an Adb.
type Device struct {
	server     server
	descriptor DeviceDescriptor

	// Used to get device info.
	deviceListFunc func() ([]*DeviceInfo, error)
}

func (c *Device) String() string {
	return c.descriptor.String()
}

// get-product is documented, but not implemented, in the server.
// TODO(z): Make product exported if get-product is ever implemented in adb.
func (c *Device) product() (string, error) {
	attr, err := c.getAttribute("get-product")
	return attr, wrapClientError(err, c, "Product")
}

func (c *Device) Serial() (string, error) {
	attr, err := c.getAttribute("get-serialno")
	return attr, wrapClientError(err, c, "Serial")
}

func (c *Device) DevicePath() (string, error) {
	attr, err := c.getAttribute("get-devpath")
	return attr, wrapClientError(err, c, "DevicePath")
}

func (c *Device) State() (DeviceState, error) {
	attr, err := c.getAttribute("get-state")
	if err != nil {
		if strings.Contains(err.Error(), "unauthorized") {
			return StateUnauthorized, nil
		}
		return StateInvalid, wrapClientError(err, c, "State")
	}
	state, err := parseDeviceState(attr)
	return state, wrapClientError(err, c, "State")
}

func (c *Device) DeviceInfo() (*DeviceInfo, error) {
	// Adb doesn't actually provide a way to get this for an individual device,
	// so we have to just list devices and find ourselves.

	serial, err := c.Serial()
	if err != nil {
		return nil, wrapClientError(err, c, "GetDeviceInfo(GetSerial)")
	}

	devices, err := c.deviceListFunc()
	if err != nil {
		return nil, wrapClientError(err, c, "DeviceInfo(ListDevices)")
	}

	for _, deviceInfo := range devices {
		if deviceInfo.Serial == serial {
			return deviceInfo, nil
		}
	}

	err = errors.Errorf(errors.DeviceNotFound, "device list doesn't contain serial %s", serial)
	return nil, wrapClientError(err, c, "DeviceInfo")
}

/*
RunCommand runs the specified commands on a shell on the device.

From the Android docs:

	Run 'command arg1 arg2 ...' in a shell on the device, and return
	its output and error streams. Note that arguments must be separated
	by spaces. If an argument contains a space, it must be quoted with
	double-quotes. Arguments cannot contain double quotes or things
	will go very wrong.

	Note that this is the non-interactive version of "adb shell"

Source: https://android.googlesource.com/platform/system/core/+/master/adb/SERVICES.TXT

This method quotes the arguments for you, and will return an error if any of them
contain double quotes.
*/
func (c *Device) RunCommand(cmd string, args ...string) (string, error) {
	cmd, err := prepareCommandLine(cmd, args...)
	if err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}

	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}
	defer conn.Close()

	req := fmt.Sprintf("shell:%s", cmd)

	// Shell responses are special, they don't include a length header.
	// We read until the stream is closed.
	// So, we can't use conn.RoundTripSingleResponse.
	if err = conn.SendMessage([]byte(req)); err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}
	if _, err = conn.ReadStatus(req); err != nil {
		return "", wrapClientError(err, c, "RunCommand")
	}

	resp, err := conn.ReadUntilEof()
	return string(resp), wrapClientError(err, c, "RunCommand")
}

func (c *Device) RunCommandContext(ctx context.Context, cmd string, args ...string) (string, error) {
	preparedCmd, err := prepareCommandLine(cmd, args...)
	if err != nil {
		return "", fmt.Errorf("prepareCommandLine err: %w", err)
	}

	conn, err := c.dialContext(ctx)
	if err != nil {
		return "", fmt.Errorf("dialContext err: %w", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("shell:%s", preparedCmd)

	// Shell responses are special, they don't include a length header.
	// We read until the stream is closed.
	// So, we can't use conn.RoundTripSingleResponse.
	if err = conn.SendMessage([]byte(req)); err != nil {
		return "", fmt.Errorf("conn.SendMessage err: %w", err)
	}
	if _, err = conn.ReadStatus(req); err != nil {
		return "", fmt.Errorf("conn.ReadStatus err: %w", err)
	}

	cmdDone := make(chan struct{})
	// Listen for context cancellation or command completion
	go func() {
		select {
		case <-cmdDone:
			return
		case <-ctx.Done():
		}

		// Find proc and kill. Remove path in cmd.
		components := strings.Split(cmd, "/")
		process := components[len(components)-1]
		fullCommand := process
		if len(args) > 0 {
			argsString := strings.Join(args, " ")
			fullCommand = fmt.Sprintf("%s %s", process, argsString)
		}

		procFetchCmd := fmt.Sprintf("ps -f -A | grep '%s' | sed 's/   */ /g' | cut -d ' ' -f 2 | head -n 1", fullCommand)
		out, err := c.RunCommand(procFetchCmd)
		if err != nil {
			log.Printf("failed to fetch matching processes: %+v", err)
			return
		}
		out = strings.TrimSpace(out)
		if len(out) == 0 {
			log.Println("output from kill was empty")
			return
		}
		log.Printf("Killing PID %s", out)
		if _, err = c.RunCommand(fmt.Sprintf("kill %s", out)); err != nil {
			log.Printf("failed to kill: %+v", err)
		}
	}()
	defer func() { close(cmdDone) }()

	resp, err := conn.ReadUntilEof()
	switch true {
	// Was there an error because context expired or was cancelled?
	case ctx.Err() != nil:
		return string(resp), ctx.Err()
	case err != nil:
		return string(resp), fmt.Errorf("conn.ReadUntilEof err: %w", err)
	default:
		return string(resp), nil
	}
}

/*
Remount, from the official adb command’s docs:

	Ask adbd to remount the device's filesystem in read-write mode,
	instead of read-only. This is usually necessary before performing
	an "adb sync" or "adb push" request.
	This request may not succeed on certain builds which do not allow
	that.

Source: https://android.googlesource.com/platform/system/core/+/master/adb/SERVICES.TXT
*/
func (c *Device) Remount() (string, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return "", wrapClientError(err, c, "Remount")
	}
	defer conn.Close()

	resp, err := conn.RoundTripSingleResponse([]byte("remount"))
	return string(resp), wrapClientError(err, c, "Remount")
}

func (c *Device) ListDirEntries(path string) (*DirEntries, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "ListDirEntries(%s)", path)
	}

	entries, err := listDirEntries(conn, path)
	return entries, wrapClientError(err, c, "ListDirEntries(%s)", path)
}

func (c *Device) Stat(path string) (*DirEntry, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "Stat(%s)", path)
	}
	defer conn.Close()

	entry, err := stat(conn, path)
	return entry, wrapClientError(err, c, "Stat(%s)", path)
}

func (c *Device) OpenRead(path string) (io.ReadCloser, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "OpenRead(%s)", path)
	}

	reader, err := receiveFile(conn, path)
	return reader, wrapClientError(err, c, "OpenRead(%s)", path)
}

// OpenWrite opens the file at path on the device, creating it with the permissions specified
// by perms if necessary, and returns a writer that writes to the file.
// The files modification time will be set to mtime when the WriterCloser is closed. The zero value
// is TimeOfClose, which will use the time the Close method is called as the modification time.
func (c *Device) OpenWrite(path string, perms os.FileMode, mtime time.Time) (io.WriteCloser, error) {
	conn, err := c.getSyncConn()
	if err != nil {
		return nil, wrapClientError(err, c, "OpenWrite(%s)", path)
	}

	writer, err := sendFile(conn, path, perms, mtime)
	return writer, wrapClientError(err, c, "OpenWrite(%s)", path)
}

// getAttribute returns the first message returned by the server by running
// <host-prefix>:<attr>, where host-prefix is determined from the DeviceDescriptor.
func (c *Device) getAttribute(attr string) (string, error) {
	resp, err := roundTripSingleResponse(c.server,
		fmt.Sprintf("%s:%s", c.descriptor.getHostPrefix(), attr))
	if err != nil {
		return "", err
	}
	return string(resp), nil
}

func (c *Device) getSyncConn() (*wire.SyncConn, error) {
	conn, err := c.dialDevice()
	if err != nil {
		return nil, err
	}

	// Switch the connection to sync mode.
	if err := wire.SendMessageString(conn, "sync:"); err != nil {
		return nil, err
	}
	if _, err := conn.ReadStatus("sync"); err != nil {
		return nil, err
	}

	return conn.NewSyncConn(), nil
}

// dialDevice switches the connection to communicate directly with the device
// by requesting the transport defined by the DeviceDescriptor.
func (c *Device) dialDevice() (*wire.Conn, error) {
	conn, err := c.server.Dial()
	if err != nil {
		return nil, err
	}

	req := fmt.Sprintf("host:%s", c.descriptor.getTransportDescriptor())
	if err = wire.SendMessageString(conn, req); err != nil {
		conn.Close()
		return nil, errors.WrapErrf(err, "error connecting to device '%s'", c.descriptor)
	}

	if _, err = conn.ReadStatus(req); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

func (c *Device) dialContext(ctx context.Context) (*wire.Conn, error) {
	conn, err := c.server.DialContext(ctx)
	if err != nil {
		return nil, err
	}

	req := fmt.Sprintf("host:%s", c.descriptor.getTransportDescriptor())
	if err = wire.SendMessageString(conn, req); err != nil {
		conn.Close()
		return nil, errors.WrapErrf(err, "error connecting to device '%s'", c.descriptor)
	}

	if _, err = conn.ReadStatus(req); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

// prepareCommandLine validates the command and argument strings, quotes
// arguments if required, and joins them into a valid adb command string.
func prepareCommandLine(cmd string, args ...string) (string, error) {
	if isBlank(cmd) {
		return "", errors.AssertionErrorf("command cannot be empty")
	}

	for i, arg := range args {
		if strings.ContainsRune(arg, '"') {
			return "", errors.Errorf(errors.ParseError, "arg at index %d contains an invalid double quote: %s", i, arg)
		}
		if containsWhitespace(arg) {
			args[i] = fmt.Sprintf("\"%s\"", arg)
		}
	}

	// Prepend the command to the args array.
	if len(args) > 0 {
		cmd = fmt.Sprintf("%s %s", cmd, strings.Join(args, " "))
	}

	return cmd, nil
}

func roundTripUntilEof(conn *wire.Conn, req string) ([]byte, error) {
	var err error
	if err = conn.SendMessage([]byte(req)); err != nil {
		return nil, fmt.Errorf("SendMessage failed: %w", err)
	}
	if _, err = conn.ReadStatus(req); err != nil {
		return nil, fmt.Errorf("ReadStatus failed: %w", err)
	}
	resp, err := conn.ReadUntilEof()
	if err != nil {
		return nil, fmt.Errorf("ReadUntilEof failed: %w", err)
	}
	return resp, nil
}

var ErrNoOp = stderr.New("no-op")

/*
Root restarts adbd in root

Corresponds to the command:

	adb root
*/
func (c *Device) Root() error {
	conn, err := c.dialDevice()
	if err != nil {
		return fmt.Errorf("dialDevice failed: %w", err)
	}
	defer conn.Close()

	resp, err := roundTripUntilEof(conn, "root:")
	if err != nil {
		return fmt.Errorf("roundTripUntilEof failed: %w", err)
	}
	respStr := string(resp)
	if strings.Contains(respStr, "restarting adbd as root") {
		return nil
	} else if strings.Contains(respStr, "adbd is already running as root") {
		return ErrNoOp
	} else {
		return fmt.Errorf("unexpected response: %s", respStr)
	}
}

/*
Unroot restarts adbd as non-root

Corresponds to the command:

	adb unroot
*/
func (c *Device) Unroot() error {
	conn, err := c.dialDevice()
	if err != nil {
		return fmt.Errorf("dialDevice failed: %w", err)
	}
	defer conn.Close()

	resp, err := roundTripUntilEof(conn, "unroot:")
	if err != nil {
		return fmt.Errorf("roundTripUntilEof failed: %w", err)
	}
	respStr := string(resp)
	if strings.Contains(respStr, "restarting adbd as non root") {
		return nil
	} else if strings.Contains(respStr, "adbd not running as root") {
		return ErrNoOp
	} else {
		return fmt.Errorf("unexpected unroot response: %s", respStr)
	}
}

type DeviceConnectionState string

const (
	DeviceConnected    = "device"
	DeviceDisconnected = "disconnect"
)

func (c *Device) WaitFor(state DeviceConnectionState) error {
	conn, err := c.server.Dial()
	if err != nil {
		return fmt.Errorf("server Dial: %w", err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("%s:wait-for-any-%s", c.descriptor.getHostPrefix(), state)
	resp, err := roundTripUntilEof(conn, cmd)
	if err != nil {
		return fmt.Errorf("roundTripUntilEof err: %w", err)
	}
	respStr := strings.TrimSpace(string(resp))
	if respStr != "" {
		log.Printf("Unexpected WaitFor response: %+v", respStr)
	}
	return nil
}
