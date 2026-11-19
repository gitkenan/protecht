//go:build !windows

package main

import (
	"errors"
	"time"
)

func captureForeground(now time.Time) (Observation, error) {
	return Observation{}, errors.New("foreground capture requires windows")
}
