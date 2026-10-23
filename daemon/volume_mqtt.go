package daemon

import (
	"crypto/rand"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	librespot "github.com/devgianlu/go-librespot"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	volumeMQTTInitialTimeout = time.Second
	volumeMQTTTimeout        = 3 * time.Second
	// Connect represents volume as an unsigned 16-bit value.
	volumeMQTTMaxVolume = uint32(1<<16 - 1)
)

// mqttVolumeBridge belongs to App so the subscription and latest acknowledged
// volume survive Spotify session replacements. Only read_topic updates its
// cached volume; publishing a command does not acknowledge that command.
type mqttVolumeBridge struct {
	log      librespot.Logger
	client   mqtt.Client
	setTopic string
	updates  chan uint32
	commands chan uint32
	done     chan struct{}
	stopped  chan struct{}
	ready    chan struct{}

	mu         sync.Mutex
	lastVolume *uint32
	readyOnce  sync.Once
	closeOnce  sync.Once
}

func newMQTTVolumeBridge(cfg VolumeMQTTConfig, log librespot.Logger) (*mqttVolumeBridge, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Host == "" || strings.ContainsAny(cfg.Host, "/?#") {
		return nil, fmt.Errorf("volume_over_mqtt.host must be a hostname or IP address without a URL scheme")
	}
	if cfg.Port == 0 {
		cfg.Port = 1883
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("volume_over_mqtt.port must be between 1 and 65535")
	}
	for name, topic := range map[string]string{"read_topic": cfg.ReadTopic, "set_topic": cfg.SetTopic} {
		if topic == "" || len(topic) > 65535 || !utf8.ValidString(topic) || strings.ContainsAny(topic, "+#\x00") {
			return nil, fmt.Errorf("volume_over_mqtt.%s must be a non-empty MQTT topic without wildcards", name)
		}
	}
	if cfg.ReadTopic == cfg.SetTopic {
		return nil, fmt.Errorf("volume_over_mqtt.read_topic and set_topic must differ")
	}

	bridge := &mqttVolumeBridge{
		log:      log,
		setTopic: cfg.SetTopic,
		updates:  make(chan uint32, 1),
		commands: make(chan uint32, 1),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		ready:    make(chan struct{}),
	}
	var clientID [8]byte
	if _, err := rand.Read(clientID[:]); err != nil {
		return nil, fmt.Errorf("generate MQTT client ID: %w", err)
	}
	broker := "tcp://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(fmt.Sprintf("glsp-%x", clientID)).
		SetConnectTimeout(volumeMQTTTimeout).
		SetWriteTimeout(volumeMQTTTimeout).
		SetConnectRetry(true).
		SetConnectRetryInterval(time.Second).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(10 * time.Second)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.WithError(err).Warn("lost MQTT volume connection; reconnecting")
	})
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		// A clean MQTT session needs its subscription restored on reconnect.
		token := client.Subscribe(cfg.ReadTopic, 1, func(_ mqtt.Client, message mqtt.Message) {
			bridge.receiveVolume(message.Payload())
		})
		if !token.WaitTimeout(volumeMQTTTimeout) {
			log.Warn("timed out subscribing to MQTT volume topic")
		} else if err := token.Error(); err != nil {
			log.WithError(err).Warn("failed subscribing to MQTT volume topic")
		} else {
			log.Infof("subscribed to MQTT volume topic %s on %s", cfg.ReadTopic, broker)
		}
	})
	bridge.client = mqtt.NewClient(opts)
	// Connection retries and publishing run off the player loop so an offline
	// broker cannot stall Spotify playback or volume controls.
	bridge.client.Connect()
	go bridge.publishCommands()
	return bridge, nil
}

func (b *mqttVolumeBridge) receiveVolume(payload []byte) {
	percentage, err := strconv.ParseFloat(strings.TrimSpace(string(payload)), 64)
	if err != nil || math.IsNaN(percentage) || math.IsInf(percentage, 0) || percentage < 0 || percentage > 100 {
		b.log.Warnf("ignoring invalid MQTT volume percentage: %q", payload)
		return
	}
	volume := uint32(math.Round(percentage * float64(volumeMQTTMaxVolume) / 100))
	b.mu.Lock()
	b.lastVolume = &volume
	// Serialize replacement of the single pending value, preserving MQTT order
	// without blocking the callback if no Spotify session is listening yet.
	select {
	case <-b.updates:
	default:
	}
	b.updates <- volume
	b.mu.Unlock()
	b.readyOnce.Do(func() { close(b.ready) })
}

// getVolume returns the latest acknowledged value, waiting briefly for the
// first (usually retained) read_topic message during session initialization.
func (b *mqttVolumeBridge) getVolume(timeout time.Duration) (uint32, bool) {
	if volume, ok := b.currentVolume(); ok {
		return volume, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-b.ready:
		return b.currentVolume()
	case <-timer.C:
		return 0, false
	case <-b.done:
		return 0, false
	}
}

func (b *mqttVolumeBridge) currentVolume() (uint32, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastVolume == nil {
		return 0, false
	}
	return *b.lastVolume, true
}

func (b *mqttVolumeBridge) setVolume(volume uint32) {
	// Coalesce rapid slider changes. Publishers may include the API handler
	// without a session as well as a player that is being replaced.
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.commands:
	default:
	}
	b.commands <- min(volume, volumeMQTTMaxVolume)
}

func (b *mqttVolumeBridge) publishCommands() {
	defer close(b.stopped)
	for {
		select {
		case <-b.done:
			return
		case volume := <-b.commands:
			percentage := math.Round(float64(volume)*10000/float64(volumeMQTTMaxVolume)) / 100
			// Commands are deliberately not retained: reconnecting a controller
			// must not replay a stale set request. QoS 1 acknowledges delivery.
			token := b.client.Publish(b.setTopic, 1, false, strconv.FormatFloat(percentage, 'f', -1, 64))
			select {
			case <-token.Done():
				if err := token.Error(); err != nil {
					b.log.WithError(err).Warn("failed publishing MQTT volume command")
				}
			case <-b.done:
				return
			}
		}
	}
}

func (b *mqttVolumeBridge) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		b.client.Disconnect(250)
		<-b.stopped
	})
	return nil
}
