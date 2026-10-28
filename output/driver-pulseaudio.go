package output

import (
	"fmt"
	"io"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

type pulseAudioOutput struct {
	log librespot.Logger

	sampleRate           int
	reader               librespot.Float32Reader
	client               *pulse.Client
	stream               *pulse.PlaybackStream
	volume               proto.Volume
	volumeLock           sync.Mutex
	externalVolume       bool
	externalVolumeUpdate chan float32
	err                  chan error
}

func newPulseAudioOutput(opts *NewOutputOptions) (*pulseAudioOutput, error) {
	// Initialize the PulseAudio client.
	// The device name is shown by PulseAudio volume controls (usually built
	// into a desktop environment), so we might want to use device_name here.
	// We could also maybe change the application icon name by device_type.
	clientopts := []pulse.ClientOption{pulse.ClientApplicationName("go-librespot"), pulse.ClientApplicationIconName("speaker")}

	if opts.RuntimeSocket != "" {
		clientopts = append(clientopts, pulse.ClientServerString(opts.RuntimeSocket))
	}

	client, err := pulse.NewClient(clientopts...)
	if err != nil {
		return nil, err
	}
	out := &pulseAudioOutput{
		log:                  opts.Log,
		sampleRate:           opts.SampleRate,
		reader:               opts.Reader,
		client:               client,
		externalVolume:       opts.ExternalVolume,
		externalVolumeUpdate: opts.VolumeUpdate,
		err:                  make(chan error, 2),
	}

	// Create a new playback.
	var channelOpt pulse.PlaybackOption
	switch opts.ChannelCount {
	case 1:
		channelOpt = pulse.PlaybackMono
	case 2:
		channelOpt = pulse.PlaybackStereo
	default:
		return nil, fmt.Errorf("cannot play %d channels, pulse only supports mono and stereo", opts.ChannelCount)
	}
	volumeUpdates := make(chan proto.ChannelVolumes, 1)
	lplaybackopts := []pulse.PlaybackOption{
		pulse.PlaybackSampleRate(out.sampleRate),
		pulse.PlaybackVolumeChanges(volumeUpdates),
		channelOpt,
	}
	if opts.ExternalVolume {
		// The external controller owns the listening level. Explicitly bypass
		// any saved PulseAudio stream attenuation, without reporting unity as
		// a volume command to that controller.
		lplaybackopts = append(lplaybackopts, pulse.PlaybackRawOption(func(req *proto.CreatePlaybackStream) {
			req.ChannelVolumes = make(proto.ChannelVolumes, opts.ChannelCount)
			for i := range req.ChannelVolumes {
				req.ChannelVolumes[i] = proto.NormVolume(1)
			}
			req.VolumeSet = true
			// Keep unity relative to the sink, including with flat volumes.
			req.RelativeVolume = true
		}))
	}

	if opts.Device != "" {
		var lsink *pulse.Sink
		if opts.Device == "default" {
			lsink, err = client.DefaultSink()
		} else {
			lsink, err = client.SinkByID(opts.Device)
		}

		if err != nil {
			client.Close()
			return nil, fmt.Errorf("cannot find pulseaudio sink %s: %w", opts.Device, err)
		}

		lplaybackopts = append(lplaybackopts, pulse.PlaybackSink(lsink))
	}

	out.stream, err = out.client.NewPlayback(pulse.Float32Reader(out.float32Reader), lplaybackopts...)
	if err != nil {
		return nil, err
	}

	// When PulseAudio controls the listening level, read its initial volume.
	// PulseAudio strongly recommends against setting a default volume at
	// startup (especially if it's 100%), so instead we just follow the
	// PulseAudio provided volume.
	if !out.externalVolume {
		cvol, _ := out.stream.Volume()
		out.volume = cvol.Avg()
		sendVolumeUpdate(opts.VolumeUpdate, float32(out.volume.Norm()))
	}

	// Listen for volume changes (through the volume mixer application, usually
	// built into the desktop environment), and send them back to Spotify.
	go func() {
		for cvol := range volumeUpdates {
			if out.externalVolume {
				// PulseAudio's stream level is independent of the external
				// listening level and must never be sent to the controller.
				continue
			}
			volume := cvol.Avg()

			out.volumeLock.Lock()
			if volume != out.volume {
				sendVolumeUpdate(opts.VolumeUpdate, float32(volume.Norm()))
				out.volume = volume
			}
			out.volumeLock.Unlock()
		}
	}()

	return out, nil
}

func (out *pulseAudioOutput) float32Reader(buf []float32) (int, error) {
	n, err := out.reader.Read(buf)
	if err != nil {
		if err == io.EOF {
			// Might happen, so translate this error message.
			return n, pulse.EndOfData
		}

		// Encountered another error. This will result in a stopped player, so
		// send the error back to the player using a non-blocking send.
		select {
		case out.err <- err:
		default:
		}
		return n, err
	}
	return n, err
}

func (out *pulseAudioOutput) Pause() error {
	if out.stream.Running() {
		// Stop() will stop new samples from being requested, but will continue
		// to play whatever is in the buffer.
		out.stream.Stop()

		// To really stop playback *now*, we have to also flush everything
		// that's in the buffer.
		err := out.client.RawRequest(&proto.FlushPlaybackStream{
			StreamIndex: out.stream.StreamIndex(),
		}, nil)
		if err != nil {
			return fmt.Errorf("Pause: could not flush playback: %e", err)
		}
	} else {
		// Nothing to do: we're already paused.
	}

	return nil
}

func (out *pulseAudioOutput) Resume() error {
	// Start the stream. This will start reading samples from out.reader and
	// push it to PulseAudio. It will do nothing if the playback is already
	// started.
	out.stream.Start()
	return nil
}

func (out *pulseAudioOutput) Drop() error {
	if out.stream.Running() {
		// Stop and flush the buffer. We do not restart here: the caller
		// resumes once the new source is set. Restarting raced with the
		// source switch and clipped the start of the track (#292).
		out.stream.Stop()
		err := out.client.RawRequest(&proto.FlushPlaybackStream{
			StreamIndex: out.stream.StreamIndex(),
		}, nil)
		if err != nil {
			return fmt.Errorf("Drop: could not flush playback: %e", err)
		}
	} else {
		// Already stopped, e.g. flushed in Pause().
	}
	return nil
}

func (out *pulseAudioOutput) DelayMs() (int64, error) {
	samples := out.stream.BufferSize()
	delay := int64(samples) * 1000 / int64(out.sampleRate)
	return delay, nil
}

func (out *pulseAudioOutput) SetVolume(vol float32) {
	if out.externalVolume {
		// The daemon forwards this request to the external controller.
		// Leave the playback stream at unity gain.
		return
	}
	volume := proto.NormVolume(float64(vol))

	out.volumeLock.Lock()
	if volume == out.volume {
		out.volumeLock.Unlock()
		return
	}
	out.volume = volume
	sendVolumeUpdate(out.externalVolumeUpdate, vol)
	out.volumeLock.Unlock()

	cvol := proto.ChannelVolumes{volume}
	err := out.stream.SetVolume(cvol)
	if err != nil {
		out.log.WithError(err).Warn("failed to set volume")
	}
}

func (out *pulseAudioOutput) Error() <-chan error {
	return out.err
}

func (out *pulseAudioOutput) Close() error {
	out.stream.Close()
	out.client.Close()
	return nil
}
