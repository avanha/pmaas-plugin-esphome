package esphome

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/common"
)

// What guards a switch on the page, and how many wrong passwords it takes to lock it.
const (
	maxPasswordFailures = 5
	passwordLockout     = 30 * time.Second
)

var (
	errReadOnly             = errors.New("it's read-only")
	errConfirmationRequired = errors.New("it has to be confirmed")
	errWrongPassword        = errors.New("wrong password")
)

// passwordFailures is the wrong passwords given for a device's switches since one was right. They count against the
// device and not the switch, so that trying each of its switches in turn doesn't give more tries.
type passwordFailures struct {
	count       int
	lockedUntil time.Time
}

// controlOf is how a device's switches are guarded. A device that isn't mentioned in the configuration isn't.
func (r *registry) controlOf(deviceName string) SwitchControl {
	return r.control[deviceName]
}

// authorizeFromPage is whether a person at the page may turn a switch of the device on or off, given what they did
// when asked. It's what ControlConfirm and ControlPassword are, and it runs on the server, so what the page does or
// doesn't ask makes no difference to what's allowed. Runs on the plugin goroutine.
func (r *registry) authorizeFromPage(deviceName string, switchName string, confirmation common.Confirmation) error {
	control := r.controlOf(deviceName)

	switch control.Level {
	case ControlOpen:
		return nil
	case ControlConfirm:
		if !confirmation.Confirmed {
			return fmt.Errorf("unable to change %q: %w", switchName, errConfirmationRequired)
		}

		return nil
	case ControlPassword:
		return r.checkPassword(deviceName, switchName, control.Password, confirmation.Password)
	default:
		return fmt.Errorf("unable to change %q: %w", switchName, errReadOnly)
	}
}

func (r *registry) checkPassword(deviceName string, switchName string, want string, got string) error {
	now := r.now()

	failures := r.failures[deviceName]
	if failures != nil && now.Before(failures.lockedUntil) {
		return fmt.Errorf("unable to change %q: too many wrong passwords, try again in %d seconds",
			switchName, int(failures.lockedUntil.Sub(now).Round(time.Second).Seconds()))
	}

	// Both are hashed first, so that comparing them takes the same time whatever the lengths, and whatever the first
	// difference is.
	wantSum, gotSum := sha256.Sum256([]byte(want)), sha256.Sum256([]byte(got))

	if subtle.ConstantTimeCompare(wantSum[:], gotSum[:]) == 1 {
		delete(r.failures, deviceName)

		return nil
	}

	if r.failures == nil {
		r.failures = make(map[string]*passwordFailures)
	}

	if failures == nil {
		failures = &passwordFailures{}
		r.failures[deviceName] = failures
	}

	failures.count++

	// What was typed isn't logged, here or anywhere.
	r.logf(slog.LevelWarn, "%s: wrong password for %q from the page (%d of %d)", deviceName, switchName, failures.count, maxPasswordFailures)

	if failures.count >= maxPasswordFailures {
		failures.count = 0
		failures.lockedUntil = now.Add(passwordLockout)
		r.logf(slog.LevelWarn, "%s: too many wrong passwords, its switches are locked for %s", deviceName, passwordLockout)
	}

	return fmt.Errorf("unable to change %q: %w", switchName, errWrongPassword)
}
