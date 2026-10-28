//go:build test_integration

package output

import (
	"fmt"
	"os"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/jfreymuth/pulse/proto"
	"github.com/stretchr/testify/require"
)

type pulseTestReader struct{}

func (pulseTestReader) Read(samples []float32) (int, error) {
	for i := range samples {
		samples[i] = 0.25
	}
	return len(samples), nil
}

// Use a dedicated PulseAudio server with a null sink: these tests change stream
// volumes, and must never run against the user's default audio server.
func TestPulseAudioVolume(t *testing.T) {
	server := os.Getenv("GO_LIBRESPOT_TEST_PULSE_SERVER")
	if server == "" {
		t.Skip("set GO_LIBRESPOT_TEST_PULSE_SERVER to a dedicated PulseAudio test server")
	}
	for _, channels := range []int{1, 2} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("channels=%d/external=%t", channels, external), func(t *testing.T) {
				updates := make(chan float32, 1)
				opts := &NewOutputOptions{
					Log: &librespot.NullLogger{}, Reader: pulseTestReader{},
					RuntimeSocket: server, SampleRate: 44100, ChannelCount: channels,
					InitialVolume: 0.3, ExternalVolume: external, VolumeUpdate: updates,
				}
				open := func() *pulseAudioOutput {
					out, err := newPulseAudioOutput(opts)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, out.Close()) })
					return out
				}
				out := open()
				if external {
					assertNoPulseVolumeUpdate(t, updates)
					assertPulseVolume(t, out, channels, 1)
				} else {
					cvol, err := out.stream.Volume()
					require.NoError(t, err)
					assertPulseVolumeUpdate(t, updates, float32(cvol.Avg().Norm()))
				}

				// Spotify requests must affect only the external controller in
				// external mode, including mute and full-volume requests.
				for _, volume := range []float32{0.3, 0, 1, 0.6} {
					out.SetVolume(volume)
					if external {
						assertPulseVolume(t, out, channels, 1)
						assertNoPulseVolumeUpdate(t, updates)
					} else {
						assertPulseVolume(t, out, channels, volume)
						assertPulseVolumeUpdate(t, updates, volume)
					}
				}
				if external {
					samples := make([]float32, 8)
					n, err := out.float32Reader(samples)
					require.NoError(t, err)
					require.Equal(t, len(samples), n)
					for _, sample := range samples {
						require.Equal(t, float32(0.25), sample)
					}
				}

				// Changes from the PulseAudio mixer must not become MQTT set
				// commands. Leave a saved attenuated volume for the next open.
				cvol := make(proto.ChannelVolumes, channels)
				for i := range cvol {
					cvol[i] = proto.NormVolume(0.4)
				}
				require.NoError(t, out.stream.SetVolume(cvol))
				if external {
					assertNoPulseVolumeUpdate(t, updates)
				} else {
					assertPulseVolumeUpdate(t, updates, 0.4)
				}
				require.NoError(t, out.Close())
				if external {
					out = open()
					assertPulseVolume(t, out, channels, 1)
					assertNoPulseVolumeUpdate(t, updates)
				}
			})
		}
	}
}

func assertPulseVolume(t *testing.T, out *pulseAudioOutput, channels int, want float32) {
	t.Helper()
	cvol, err := out.stream.Volume()
	require.NoError(t, err)
	require.Len(t, cvol, channels)
	for _, volume := range cvol {
		require.Equal(t, proto.NormVolume(float64(want)), volume)
	}
}

func assertNoPulseVolumeUpdate(t *testing.T, updates <-chan float32) {
	t.Helper()
	select {
	case volume := <-updates:
		t.Fatalf("PulseAudio stream volume leaked to external controller: %v", volume)
	case <-time.After(100 * time.Millisecond):
	}
}

func assertPulseVolumeUpdate(t *testing.T, updates <-chan float32, want float32) {
	t.Helper()
	select {
	case volume := <-updates:
		require.InDelta(t, want, volume, 0.0001)
	case <-time.After(time.Second):
		t.Fatal("missing PulseAudio volume update")
	}
}
