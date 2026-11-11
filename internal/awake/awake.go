// Package awake stops the computer from idle-sleeping while Continuum has a
// resume coming up or a resumed session is still working. A locked screen does
// not stop Continuum (nothing it does needs the display), but a sleeping
// computer runs nothing, so it asks the OS to stay awake for that long.
//
//   - macOS:   caffeinate -i -s -w <daemon pid> (idle sleep; system sleep on AC)
//   - Linux:   systemd-inhibit --what=idle:sleep, tied to the daemon's lifetime
//   - Windows: SetThreadExecutionState(ES_SYSTEM_REQUIRED)
//
// Closing a laptop lid on battery still sleeps; that is the OS's call.
package awake

import "sync"

// Keeper holds or releases the stay-awake request.
type Keeper struct {
	mu     sync.Mutex
	held   bool
	reason string
	impl   holder
}

type holder interface {
	hold(reason string) error
	release()
}

func New() *Keeper { return &Keeper{impl: newHolder()} }

// Set holds the request when on is true and releases it otherwise. It
// returns whether the state changed and any error from the OS.
func (k *Keeper) Set(on bool, reason string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if on == k.held {
		return false, nil
	}
	if on {
		if err := k.impl.hold(reason); err != nil {
			return false, err
		}
	} else {
		k.impl.release()
	}
	k.held, k.reason = on, reason
	return true, nil
}

// Held reports whether the request is held.
func (k *Keeper) Held() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.held
}
