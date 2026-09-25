package daemon

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

const (
	volumeTCPGetTimeout = 300 * time.Millisecond
	volumeTCPSetTimeout = 200 * time.Millisecond
	volumeTCPMaxVolume  = uint32(1<<16 - 1)
)

func byteToVolume(b byte) uint32 {
	return uint32(math.Round(float64(b) * float64(volumeTCPMaxVolume) / 255.0))
}

func volumeToByte(volume uint32) byte {
	if volume > volumeTCPMaxVolume {
		volume = volumeTCPMaxVolume
	}

	return byte(math.Round(float64(volume) * 255.0 / float64(volumeTCPMaxVolume)))
}

// tcpVolumeBridge synchronizes Connect volume state with an external service.
// It belongs to App rather than AppPlayer so its listening socket survives a
// session replacement.
type tcpVolumeBridge struct {
	log            librespot.Logger
	serviceAddress string
	listener       net.Listener
	updates        chan uint32
	done           chan struct{}
}

func newTCPVolumeBridge(cfg VolumeTCPConfig, log librespot.Logger) (*tcpVolumeBridge, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	listenerAddress := net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", listenerAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", listenerAddress, err)
	}

	bridge := &tcpVolumeBridge{
		log:            log,
		serviceAddress: net.JoinHostPort(cfg.ServiceAddress, strconv.Itoa(cfg.ServicePort)),
		listener:       listener,
		updates:        make(chan uint32, 8),
		done:           make(chan struct{}),
	}

	log.Infof("volume server listening on %s", listener.Addr())
	go bridge.accept()

	return bridge, nil
}

func (b *tcpVolumeBridge) accept() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.done:
				return
			default:
				b.log.WithError(err).Warn("failed accepting external volume connection")
				continue
			}
		}

		go b.handleConn(conn)
	}
}

func (b *tcpVolumeBridge) handleConn(conn net.Conn) {
	defer conn.Close()

	var buf [1]byte
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		b.log.WithError(err).Debug("failed reading external volume update")
		return
	}

	volume := byteToVolume(buf[0])
	b.log.Debugf("got external volume update (%d) from %s", volume, conn.RemoteAddr())

	select {
	case b.updates <- volume:
		return
	default:
	}

	// Keep the most recent updates without allowing a slow player loop to
	// block connection handlers. Another producer may win either race.
	select {
	case <-b.updates:
	default:
	}
	select {
	case b.updates <- volume:
	default:
	}
}

func (b *tcpVolumeBridge) getVolume(timeout time.Duration) (uint32, bool) {
	conn, err := net.DialTimeout("tcp", b.serviceAddress, timeout)
	if err != nil {
		b.log.WithError(err).Debug("failed connecting to external volume service")
		return 0, false
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte{0x00}); err != nil {
		b.log.WithError(err).Debug("failed requesting external volume")
		return 0, false
	}

	var buf [1]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		b.log.WithError(err).Debug("failed reading external volume")
		return 0, false
	}

	return byteToVolume(buf[0]), true
}

func (b *tcpVolumeBridge) setVolume(volume uint32, timeout time.Duration) {
	value := volumeToByte(volume)

	go func() {
		select {
		case <-b.done:
			return
		default:
		}

		conn, err := net.DialTimeout("tcp", b.serviceAddress, timeout)
		if err != nil {
			b.log.WithError(err).Debug("failed connecting to external volume service")
			return
		}
		defer conn.Close()

		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write([]byte{0x01, value}); err != nil {
			b.log.WithError(err).Debug("failed reporting external volume")
		}
	}()
}

func (b *tcpVolumeBridge) Close() error {
	close(b.done)
	if err := b.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
