package engine

import "errors"

var (
	ErrPermanent      = errors.New("permanent deployment error")
	ErrImageNotReady  = errors.New("image not ready")
	ErrNeedsAttention = errors.New("a person must look at the pin")

	errSyncDeadline = errors.New("sync deadline reached")
	errPinDeadline  = errors.New("pin deadline reached")
)
