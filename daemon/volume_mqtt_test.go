//go:build test_unit

package daemon

import (
	"net"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/stretchr/testify/require"
)

// The test broker exchanges real MQTT packets, including PUBACK, without
// depending on an external broker or a second server library.
func newVolumeMQTTTestBridge(t *testing.T) (*mqttVolumeBridge, *net.TCPListener) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	bridge, err := newMQTTVolumeBridge(VolumeMQTTConfig{
		Enabled:   true,
		Host:      "127.0.0.1",
		Port:      listener.Addr().(*net.TCPAddr).Port,
		ReadTopic: "test/volume",
		SetTopic:  "test/volume/set",
	}, &librespot.NullLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	return bridge, listener.(*net.TCPListener)
}

func acceptVolumeMQTTClient(t *testing.T, listener *net.TCPListener) net.Conn {
	t.Helper()
	require.NoError(t, listener.SetDeadline(time.Now().Add(5*time.Second)))
	conn, err := listener.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	packet, err := packets.ReadPacket(conn)
	require.NoError(t, err)
	require.IsType(t, &packets.ConnectPacket{}, packet)
	require.NoError(t, packets.NewControlPacket(packets.Connack).Write(conn))
	packet, err = packets.ReadPacket(conn)
	require.NoError(t, err)
	sub, ok := packet.(*packets.SubscribePacket)
	require.True(t, ok, "expected SUBSCRIBE, got %T", packet)
	require.Equal(t, []string{"test/volume"}, sub.Topics)
	ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	ack.MessageID = sub.MessageID
	ack.ReturnCodes = []byte{1}
	require.NoError(t, ack.Write(conn))
	return conn
}

func publishVolumeMQTT(t *testing.T, conn net.Conn, payload string, retained bool) {
	t.Helper()
	message := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	message.TopicName = "test/volume"
	message.Payload = []byte(payload)
	message.Retain = retained
	require.NoError(t, message.Write(conn))
}

func TestMQTTVolumeBridgeProtocol(t *testing.T) {
	bridge, listener := newVolumeMQTTTestBridge(t)
	conn := acceptVolumeMQTTClient(t, listener)

	// A retained percentage initializes the cached Connect value.
	publishVolumeMQTT(t, conn, "37.5", true)
	volume, ok := bridge.getVolume(time.Second)
	require.True(t, ok)
	require.Equal(t, uint32(24576), volume)
	require.Equal(t, volume, <-bridge.updates)

	// Command publication does not change the acknowledged value.
	bridge.setVolume(49151)
	packet, err := packets.ReadPacket(conn)
	require.NoError(t, err)
	command, ok := packet.(*packets.PublishPacket)
	require.True(t, ok, "expected PUBLISH, got %T", packet)
	require.Equal(t, "test/volume/set", command.TopicName)
	require.Equal(t, "75", string(command.Payload))
	require.Equal(t, byte(1), command.Qos)
	require.False(t, command.Retain)
	ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
	ack.MessageID = command.MessageID
	require.NoError(t, ack.Write(conn))
	volume, ok = bridge.getVolume(0)
	require.True(t, ok)
	require.Equal(t, uint32(24576), volume)

	// The controller's acknowledgment is delivered to the Spotify loop.
	publishVolumeMQTT(t, conn, "75", false)
	select {
	case volume = <-bridge.updates:
		require.Equal(t, uint32(49151), volume)
	case <-time.After(time.Second):
		t.Fatal("MQTT acknowledgment was not delivered")
	}
	require.Empty(t, bridge.commands, "received updates must not echo a command")
	volume, ok = bridge.getVolume(0)
	require.True(t, ok)
	require.Equal(t, uint32(49151), volume)

	// A replacement MQTT connection restores the subscription and state.
	require.NoError(t, conn.Close())
	reconnected := acceptVolumeMQTTClient(t, listener)
	publishVolumeMQTT(t, reconnected, "0", true)
	select {
	case volume = <-bridge.updates:
		require.Zero(t, volume)
	case <-time.After(time.Second):
		t.Fatal("MQTT volume was not restored after reconnecting")
	}
}

func TestMQTTVolumePayloadValidationAndLatestValue(t *testing.T) {
	bridge := &mqttVolumeBridge{
		log:     &librespot.NullLogger{},
		updates: make(chan uint32, 1),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, payload := range []string{"", "NaN", "Inf", "-1", "100.01", "abc", "{\"volume\":50}"} {
		bridge.receiveVolume([]byte(payload))
		_, ok := bridge.currentVolume()
		require.False(t, ok, "payload %q must be rejected", payload)
		require.Empty(t, bridge.updates)
	}
	_, ok := bridge.getVolume(time.Millisecond)
	require.False(t, ok, "missing retained value must time out")
	for _, payload := range []string{"0", "50", " 100\n"} {
		bridge.receiveVolume([]byte(payload))
	}
	volume, ok := bridge.getVolume(0)
	require.True(t, ok)
	require.Equal(t, volumeMQTTMaxVolume, volume)
	require.Equal(t, volumeMQTTMaxVolume, <-bridge.updates, "only the latest external update should wait for a session")
}

func TestMQTTVolumeBridgeDisabledAndInvalidConfig(t *testing.T) {
	bridge, err := newMQTTVolumeBridge(VolumeMQTTConfig{}, &librespot.NullLogger{})
	require.NoError(t, err)
	require.Nil(t, bridge)
	for _, cfg := range []VolumeMQTTConfig{
		{Enabled: true},
		{Enabled: true, Host: "http://mqtt.home"},
		{Enabled: true, Host: "mqtt.home", Port: -1},
		{Enabled: true, Host: "mqtt.home", Port: 65536},
		{Enabled: true, Host: "mqtt.home", ReadTopic: "test/#", SetTopic: "test/set"},
		{Enabled: true, Host: "mqtt.home", ReadTopic: "test", SetTopic: "test"},
	} {
		bridge, err := newMQTTVolumeBridge(cfg, &librespot.NullLogger{})
		require.Error(t, err)
		require.Nil(t, bridge)
	}
}
