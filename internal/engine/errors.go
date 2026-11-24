package engine

import "errors"

var (
	ErrPermanent     = errors.New("permanent deployment error")
	ErrImageNotReady = errors.New("image not ready")

	errSyncDeadline = errors.New("sync deadline reached")
)
