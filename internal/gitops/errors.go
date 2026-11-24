package gitops

import "errors"

var (
	ErrInvalidRequest      = errors.New("invalid pin request")
	ErrPinNotFound         = errors.New("pin location not found")
	ErrAmbiguousPin        = errors.New("pin location is ambiguous")
	ErrUnsupportedYAML     = errors.New("pin location cannot be edited safely")
	ErrConflict            = errors.New("pin write kept conflicting")
	ErrBranchProtected     = errors.New("branch protection or a ruleset rejected the pin write")
	ErrPinOverwritten      = errors.New("pin landed but the branch no longer holds it")
	ErrPullRequestClosed   = errors.New("pin pull request was closed without merging")
	ErrPullRequestTampered = errors.New("pin pull request contains changes doorstep did not write")
	ErrMergeTimeout        = errors.New("pin pull request was not merged in time")
)
