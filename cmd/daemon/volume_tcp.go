package main

import (
	"fmt"
	"io"
	"math"
	"net"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/player"
)

func byteToVolume(b byte) uint32 {
	return uint32(math.Round(float64(b) * float64(player.MaxStateVolume) / 255.0))
}

func volumeToByte(v uint32) byte {
	if v > player.MaxStateVolume {
		v = player.MaxStateVolume
	}
	return byte(math.Round(float64(v) * 255.0 / float64(player.MaxStateVolume)))
}

type TCPVolumeClient struct {
	enabled bool
	address string
}

type TCPVolumeListener struct {
	enabled bool
	address string
	vol     chan uint32
}

func NewTCPVolumeClient(enabled bool, addr string, port int) *TCPVolumeClient {
	return &TCPVolumeClient{enabled: enabled, address: fmt.Sprintf("%s:%d", addr, port)}
}

// Request volume once from external service
func (c *TCPVolumeClient) GetVolume(timeout time.Duration) (uint32, bool) {
	if !c.enabled {
		return 0, false
	}

	conn, err := net.DialTimeout("tcp", c.address, timeout)
	if err != nil {
		return 0, false
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))

	// 0x00 -> Request volume
	if _, err := conn.Write([]byte{0x00}); err != nil {
		return 0, false
	}

	// Response: 1 byte volume
	var buf [1]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		return 0, false
	}

	return byteToVolume(buf[0]), true
}

// Report volume to external service
func (c *TCPVolumeClient) SetVolume(volInternal uint32, timeout time.Duration) {
	if !c.enabled {
		return
	}

	b := volumeToByte(volInternal)

	go func(val byte) {
		conn, err := net.DialTimeout("tcp", c.address, timeout)
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.SetDeadline(time.Now().Add(timeout))

		// 0x01 + vol -> Set volume
		_, _ = conn.Write([]byte{0x01, val})
	}(b)
}

func NewTCPVolumeListener(enabled bool, addr string, port int) *TCPVolumeListener {
	return &TCPVolumeListener{
		enabled: enabled,
		address: fmt.Sprintf("%s:%d", addr, port),
		vol:     make(chan uint32, 8),
	}
}

func (l *TCPVolumeListener) Start(log librespot.Logger) {
	if !l.enabled {
		return
	}
	go func() {
		ln, err := net.Listen("tcp", l.address)
		if err != nil {
			return
		}
		log.Infof("volume server listening on %s", ln.Addr())
		for {
			conn, err := ln.Accept()
			if err != nil {
				continue
			}
			go l.handleConn(log, conn)
		}
	}()
}

func (l *TCPVolumeListener) handleConn(log librespot.Logger, c net.Conn) {
	defer c.Close()

	var buf [1]byte
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf[:]); err != nil {
		return
	}

	v := byteToVolume(buf[0])
	log.Debugf("got external volume update (%d) from %s", v, c.RemoteAddr().String())

	select {
	case l.vol <- v:
	default:
		select {
		case <-l.vol:
		default:
		}
		l.vol <- v
	}
}
