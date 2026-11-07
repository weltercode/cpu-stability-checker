//go:build !windows

package whea

import "errors"

var errUnsupported = errors.New("whea: WHEA is a Windows facility")

func newQuerier() (querier, bool) { return nil, false }
