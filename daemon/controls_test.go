//go:build test_unit

package daemon

import (
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/player"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The first press of a burst loads at once, so a lone skip is as quick as it
// ever was; only a press that follows another within the window is held back.
func TestSkipDelay(t *testing.T) {
	now := time.Now()
	window := 400 * time.Millisecond

	cases := []struct {
		name     string
		debounce time.Duration
		lastSkip time.Time
		want     time.Duration
	}{
		{"disabled never delays", 0, now, 0},
		{"first skip is immediate", window, time.Time{}, 0},
		{"skip after quiet period is immediate", window, now.Add(-time.Second), 0},
		{"skip right after previous is held", window, now.Add(-100 * time.Millisecond), window},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, skipDelay(tc.debounce, now, tc.lastSkip))
		})
	}
}

// newVolumeTestApp is an App with a saved volume of saved (nil for none) whose
// saves are reported on the returned channel.
func newVolumeTestApp(t *testing.T, cfg *Config, saved *uint32) (*App, <-chan uint32) {
	t.Helper()

	saves := make(chan uint32, 8)
	store := librespot.NewMockStateStore(t)
	store.EXPECT().Save(mock.Anything).RunAndReturn(func(s *librespot.AppState) error {
		saves <- *s.LastVolume
		return nil
	}).Maybe()

	return &App{
		cfg:        cfg,
		log:        &librespot.NullLogger{},
		stateStore: store,
		state:      &librespot.AppState{LastVolume: saved},
	}, saves
}

func requireSaved(t *testing.T, saves <-chan uint32, want uint32) {
	t.Helper()
	select {
	case got := <-saves:
		require.Equal(t, want, got)
	case <-time.After(time.Second):
		t.Fatal("the volume was not saved")
	}
}

func TestSessionVolume(t *testing.T) {
	saved := uint32(player.MaxStateVolume / 4)
	cfg := func(ignoreLast bool) *Config {
		return &Config{VolumeSteps: 100, InitialVolume: 50, IgnoreLastVolume: ignoreLast}
	}

	app, _ := newVolumeTestApp(t, cfg(false), nil)
	require.Equal(t, uint32(player.MaxStateVolume/2), app.sessionVolume(), "nothing saved: initial_volume")

	app, _ = newVolumeTestApp(t, cfg(false), &saved)
	require.Equal(t, saved, app.sessionVolume(), "the volume an earlier run saved")

	app, saves := newVolumeTestApp(t, cfg(true), &saved)
	require.Equal(t, uint32(player.MaxStateVolume/2), app.sessionVolume(), "ignore_last_volume skips the saved one")

	set := uint32(player.MaxStateVolume / 3)
	app.setVolume(set)
	requireSaved(t, saves, set)
	require.Equal(t, set, app.sessionVolume(), "a volume set while running wins, ignore_last_volume or not")

	app, _ = newVolumeTestApp(t, &Config{VolumeSteps: 100, InitialVolume: 150}, nil)
	require.Equal(t, uint32(player.MaxStateVolume), app.sessionVolume(), "initial_volume above the steps is capped")
}

func TestVolumeFromApi(t *testing.T) {
	app := &App{cfg: &Config{VolumeSteps: 100}}
	current := uint32(player.MaxStateVolume * 37 / 100)

	cases := []struct {
		name string
		data ApiSetVolume
		want uint32
	}{
		{"absolute", ApiSetVolume{Volume: 64}, player.MaxStateVolume * 64 / 100},
		{"relative up", ApiSetVolume{Volume: 5, Relative: true}, player.MaxStateVolume * 42 / 100},
		{"relative down past zero", ApiSetVolume{Volume: -50, Relative: true}, 0},
		{"absolute above the steps", ApiSetVolume{Volume: 150}, player.MaxStateVolume},
		{"absolute below zero", ApiSetVolume{Volume: -1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, app.volumeFromApi(tc.data, current))
		})
	}
}

func TestMQTTVolumeWithoutSession(t *testing.T) {
	app, saves := newVolumeTestApp(t, &Config{VolumeSteps: 100, InitialVolume: 10}, nil)
	app.volumeMQTT = &mqttVolumeBridge{
		log:      app.log,
		updates:  make(chan uint32, 1),
		commands: make(chan uint32, 1),
		ready:    make(chan struct{}),
	}
	app.volumeMQTT.receiveVolume([]byte("30"))
	require.Equal(t, uint32(30), app.volumeWithoutSession().Value)
	app.setVolumeWithoutSession(ApiSetVolume{Volume: 5, Relative: true})
	require.Equal(t, uint32(player.MaxStateVolume*35/100), <-app.volumeMQTT.commands)
	requireSaved(t, saves, player.MaxStateVolume*35/100)
	// A command is not an acknowledgment; the external state stays at 30%.
	require.Equal(t, uint32(30), app.volumeWithoutSession().Value)
	app.volumeMQTT.receiveVolume([]byte("35"))
	require.Equal(t, uint32(35), app.volumeWithoutSession().Value)
}

func TestMQTTVolumePublishesToSpotifyWithoutEcho(t *testing.T) {
	p := newTestAppPlayer(t)
	app, saves := newVolumeTestApp(t, &Config{VolumeSteps: 100}, nil)
	app.server = p.app.server
	app.volumeMQTT = &mqttVolumeBridge{commands: make(chan uint32, 1)}
	p.app = app
	p.volumeUpdate = make(chan float32, 1)
	volume := uint32(49151)
	require.True(t, p.externalVolumeUpdated(volume))
	requireSaved(t, saves, volume)
	require.False(t, p.externalVolumeUpdated(volume), "duplicate acknowledgments need no new notification")

	// The debounced player-loop notification still reaches both Connect and
	// the events API, without enqueuing a second external volume command.
	p.volumeUpdated()
	pushes := drain(p.statePush)
	require.NotEmpty(t, pushes)
	var state connectpb.PutStateRequest
	require.NoError(t, proto.Unmarshal(pushes[len(pushes)-1].body, &state))
	require.Equal(t, connectpb.PutStateReason_VOLUME_CHANGED, state.PutStateReason)
	require.Equal(t, volume, state.Device.DeviceInfo.Volume)
	require.Contains(t, apiEvents(p), ApiEventTypeVolume)
	require.Empty(t, app.volumeMQTT.commands)
	require.Empty(t, p.volumeUpdate)
}
