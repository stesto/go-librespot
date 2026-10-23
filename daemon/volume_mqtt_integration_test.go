//go:build test_integration

package daemon

import (
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/stretchr/testify/require"
)

// Opt in explicitly: this test publishes a volume command to real test topics.
func TestMQTTVolumeLiveBroker(t *testing.T) {
	host := os.Getenv("LIBRESPOT_MQTT_TEST_HOST")
	if host == "" {
		t.Skip("set LIBRESPOT_MQTT_TEST_HOST to test a real MQTT broker")
	}
	cfg := VolumeMQTTConfig{
		Enabled:   true,
		Host:      host,
		Port:      1883,
		ReadTopic: "minidsp_dev/911111/Master/Volume",
		SetTopic:  "minidsp_dev/911111/Master/Volume/set",
	}
	if port := os.Getenv("LIBRESPOT_MQTT_TEST_PORT"); port != "" {
		var err error
		cfg.Port, err = strconv.Atoi(port)
		require.NoError(t, err)
	}
	bridge, err := newMQTTVolumeBridge(cfg, &librespot.NullLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	volume, ok := bridge.getVolume(5 * time.Second)
	require.True(t, ok, "no retained/current volume received on %s", cfg.ReadTopic)
	t.Logf("received current volume: %.2f%%", float64(volume)*100/float64(volumeMQTTMaxVolume))

	observer := mqtt.NewClient(mqtt.NewClientOptions().
		AddBroker("tcp://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))).
		SetClientID(fmt.Sprintf("glsp-test-%x", time.Now().UnixNano())).
		SetConnectTimeout(3 * time.Second))
	token := observer.Connect()
	require.True(t, token.WaitTimeout(5*time.Second))
	require.NoError(t, token.Error())
	t.Cleanup(func() { observer.Disconnect(250) })
	commands := make(chan string, 4)
	token = observer.Subscribe(cfg.SetTopic, 1, func(_ mqtt.Client, message mqtt.Message) {
		if !message.Retained() {
			select {
			case commands <- string(message.Payload()):
			default:
			}
		}
	})
	require.True(t, token.WaitTimeout(5*time.Second))
	require.NoError(t, token.Error())
	// Reapply the current value rounded to a whole percentage.
	bridge.setVolume(volume)
	select {
	case payload := <-commands:
		percentage, err := strconv.Atoi(payload)
		require.NoError(t, err)
		require.Equal(t, int(math.Round(float64(volume)*100/float64(volumeMQTTMaxVolume))), percentage)
		t.Logf("observed command on %s: %s", cfg.SetTopic, payload)
	case <-time.After(5 * time.Second):
		t.Fatal("MQTT volume command was not observed")
	}
}
