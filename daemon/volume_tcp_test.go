//go:build test_unit

package daemon

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/stretchr/testify/require"
)

func TestTCPVolumeConversionRoundTrip(t *testing.T) {
	for value := 0; value <= 255; value++ {
		require.Equal(t, byte(value), volumeToByte(byteToVolume(byte(value))))
	}
}

func TestTCPVolumeBridgeProtocol(t *testing.T) {
	service, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	tcpService := service.(*net.TCPListener)

	serviceHost, servicePortString, err := net.SplitHostPort(service.Addr().String())
	require.NoError(t, err)
	servicePort, err := strconv.Atoi(servicePortString)
	require.NoError(t, err)

	bridge, err := newTCPVolumeBridge(VolumeTCPConfig{
		Enabled:        true,
		Address:        "127.0.0.1",
		Port:           0,
		ServiceAddress: serviceHost,
		ServicePort:    servicePort,
	}, &librespot.NullLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })

	type getResult struct {
		volume uint32
		ok     bool
	}
	gotVolume := make(chan getResult, 1)
	go func() {
		volume, ok := bridge.getVolume(time.Second)
		gotVolume <- getResult{volume: volume, ok: ok}
	}()

	require.NoError(t, tcpService.SetDeadline(time.Now().Add(5*time.Second)))
	getConn, err := tcpService.Accept()
	require.NoError(t, err)
	var getRequest [1]byte
	_, err = io.ReadFull(getConn, getRequest[:])
	require.NoError(t, err)
	require.Equal(t, byte(0x00), getRequest[0])
	_, err = getConn.Write([]byte{93})
	require.NoError(t, err)
	require.NoError(t, getConn.Close())

	select {
	case result := <-gotVolume:
		require.True(t, result.ok)
		require.Equal(t, byteToVolume(93), result.volume)
	case <-time.After(5 * time.Second):
		t.Fatal("external volume request did not complete")
	}

	bridge.setVolume(byteToVolume(177), time.Second)
	require.NoError(t, tcpService.SetDeadline(time.Now().Add(5*time.Second)))
	setConn, err := tcpService.Accept()
	require.NoError(t, err)
	var setRequest [2]byte
	_, err = io.ReadFull(setConn, setRequest[:])
	require.NoError(t, err)
	require.Equal(t, [2]byte{0x01, 177}, setRequest)
	require.NoError(t, setConn.Close())

	updateConn, err := net.DialTimeout("tcp", bridge.listener.Addr().String(), time.Second)
	require.NoError(t, err)
	_, err = updateConn.Write([]byte{211})
	require.NoError(t, err)
	require.NoError(t, updateConn.Close())

	select {
	case volume := <-bridge.updates:
		require.Equal(t, byteToVolume(211), volume)
	case <-time.After(time.Second):
		t.Fatal("external volume update was not delivered")
	}
}
