package adb

import (
	"context"
	"testing"
	"time"

	"github.com/rakeeb-hossain/goadb/internal/errors"
	"github.com/rakeeb-hossain/goadb/wire"
	"github.com/stretchr/testify/assert"
)

func TestGetAttribute(t *testing.T) {
	s := &MockServer{
		Status:   wire.StatusSuccess,
		Messages: []string{"value"},
	}
	client := (&Adb{s}).Device(DeviceWithSerial("serial"))

	v, err := client.getAttribute("attr")
	assert.Equal(t, "host-serial:serial:attr", s.Requests[0])
	assert.NoError(t, err)
	assert.Equal(t, "value", v)
}

func TestGetDeviceInfo(t *testing.T) {
	deviceLister := func() ([]*DeviceInfo, error) {
		return []*DeviceInfo{
			&DeviceInfo{
				Serial:  "abc",
				Product: "Foo",
			},
			&DeviceInfo{
				Serial:  "def",
				Product: "Bar",
			},
		}, nil
	}

	client := newDeviceClientWithDeviceLister("abc", deviceLister)
	device, err := client.DeviceInfo()
	assert.NoError(t, err)
	assert.Equal(t, "Foo", device.Product)

	client = newDeviceClientWithDeviceLister("def", deviceLister)
	device, err = client.DeviceInfo()
	assert.NoError(t, err)
	assert.Equal(t, "Bar", device.Product)

	client = newDeviceClientWithDeviceLister("serial", deviceLister)
	device, err = client.DeviceInfo()
	assert.True(t, HasErrCode(err, DeviceNotFound))
	assert.EqualError(t, err.(*errors.Err).Cause,
		"DeviceNotFound: device list doesn't contain serial serial")
	assert.Nil(t, device)
}

func newDeviceClientWithDeviceLister(serial string, deviceLister func() ([]*DeviceInfo, error)) *Device {
	client := (&Adb{&MockServer{
		Status:   wire.StatusSuccess,
		Messages: []string{serial},
	}}).Device(DeviceWithSerial(serial))
	client.deviceListFunc = deviceLister
	return client
}

func TestRunCommandNoArgs(t *testing.T) {
	s := &MockServer{
		Status:   wire.StatusSuccess,
		Messages: []string{"output"},
	}
	client := (&Adb{s}).Device(AnyDevice())

	v, err := client.RunCommand("cmd")
	assert.Equal(t, "host:transport-any", s.Requests[0])
	assert.Equal(t, "shell:cmd", s.Requests[1])
	assert.NoError(t, err)
	assert.Equal(t, "output", v)
}

func TestPrepareCommandLineNoArgs(t *testing.T) {
	result, err := prepareCommandLine("cmd")
	assert.NoError(t, err)
	assert.Equal(t, "cmd", result)
}

func TestPrepareCommandLineEmptyCommand(t *testing.T) {
	_, err := prepareCommandLine("")
	assert.Equal(t, errors.AssertionError, code(err))
	assert.Equal(t, "command cannot be empty", message(err))
}

func TestPrepareCommandLineBlankCommand(t *testing.T) {
	_, err := prepareCommandLine("  ")
	assert.Equal(t, errors.AssertionError, code(err))
	assert.Equal(t, "command cannot be empty", message(err))
}

func TestPrepareCommandLineCleanArgs(t *testing.T) {
	result, err := prepareCommandLine("cmd", "arg1", "arg2")
	assert.NoError(t, err)
	assert.Equal(t, "cmd arg1 arg2", result)
}

func TestPrepareCommandLineArgWithWhitespaceQuotes(t *testing.T) {
	result, err := prepareCommandLine("cmd", "arg with spaces")
	assert.NoError(t, err)
	assert.Equal(t, "cmd \"arg with spaces\"", result)
}

func TestPrepareCommandLineArgWithDoubleQuoteFails(t *testing.T) {
	_, err := prepareCommandLine("cmd", "quoted\"arg")
	assert.Equal(t, errors.ParseError, code(err))
	assert.Equal(t, "arg at index 0 contains an invalid double quote: quoted\"arg", message(err))
}

func code(err error) errors.ErrCode {
	return err.(*errors.Err).Code
}

func message(err error) string {
	return err.(*errors.Err).Message
}

func TestDevice_WaitFor(t *testing.T) {
	client, err := New()
	assert.Nil(t, err)

	device := client.Device(DeviceWithSerial("172.31.27.64:5555"))
	err = device.WaitFor(context.Background(), DeviceConnected)
	assert.Nil(t, err)
}

func TestDevice_RunCommandContext(t *testing.T) {
	client, _ := New()
	device := client.Device(DeviceWithSerial("127.0.0.1:6555"))

	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	out, err := device.RunCommandContext(ctx, `while true; do echo "test"; sleep 1; done`)
	assert.NotNil(t, err)
	assert.Equal(t, ctx.Err(), err)
	assert.NotEqual(t, "", out)
	println(out)
}

func TestDevice_RunCommandFridaServerContext(t *testing.T) {
	client, _ := New()
	device := client.Device(DeviceWithSerial("127.0.0.1:6555"))

	for range 10 {
		ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
		out, err := device.RunCommandContext(ctx, `/data/local/frida-server`)
		assert.NotNil(t, err)
		assert.Equal(t, ctx.Err(), err)
		println(out)
	}
}
