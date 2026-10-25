//go:build !linux

package voice

import (
	"context"
	"errors"
)

var ErrBusy = errors.New("诊断接口被占用")
var errUnsupported = errors.New("浏览器 USB 收听目前需要 Linux 主机")

type Device struct{ Path, USB, Generation string }
type Stream struct{}

func Resolve(string) (Device, error)                      { return Device{}, errUnsupported }
func Open(string) (*Stream, error)                        { return nil, errUnsupported }
func (*Stream) Read(context.Context, []byte) (int, error) { return 0, errUnsupported }
func (*Stream) Close() error                              { return nil }

func (*Stream) write([]byte) error { return errUnsupported }
