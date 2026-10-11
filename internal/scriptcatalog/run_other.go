//go:build !linux

package scriptcatalog

import (
	"context"
	"errors"
)

func Run(context.Context, Entry, []byte, int) (Result, error) {
	return Result{}, errors.New("script catalog execution is unsupported on this OS")
}
