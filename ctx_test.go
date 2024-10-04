package adb

import (
	"context"
	"github.com/stretchr/testify/assert"
	"testing"
	"time"
)

func TestContextCancellation(t *testing.T) {
	client, err := New()
	assert.Nil(t, err)

	device := client.Device(DeviceWithSerial("172.31.27.64:5555"))
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := device.RunCommandContext(ctx, "while ! [[ -z $(getprop sys.boot_completed) ]]; do sleep 1; done;")
		if err != nil {
			panic(err)
		}
		done <- struct{}{}
	}()

	time.Sleep(time.Second * 1)
	cancel()
	<-done
}
