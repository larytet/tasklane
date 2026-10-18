package webauthn

import (
	"errors"
	"math"
)

// A small CBOR reader (RFC 8949) for what WebAuthn carries: attestation objects, authenticator data and COSE keys. It reads
// definite-length items only (CTAP2 canonical form), bounds depth and sizes, and never allocates from a length the input has not
// backed with bytes.

var errCBOR = errors.New("malformed CBOR")

const (
	maxDepth = 8
	maxItems = 1024
)

// Decode reads one item from the start of b and returns it with the number of bytes it used. Values are: uint64 or int64 (as
// int64 when it fits), []byte, string, []any, map[any]any (keys are int64 or string), bool, nil.
func Decode(b []byte) (any, int, error) {
	d := &decoder{b: b}
	v, err := d.item(0)
	return v, d.i, err
}

type decoder struct {
	b []byte
	i int
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || n > len(d.b)-d.i {
		return nil, errCBOR
	}
	out := d.b[d.i : d.i+n]
	d.i += n
	return out, nil
}

func (d *decoder) head() (major byte, arg uint64, err error) {
	h, err := d.take(1)
	if err != nil {
		return 0, 0, err
	}
	major, info := h[0]>>5, h[0]&0x1f
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info <= 27:
		n := 1 << (info - 24)
		bs, err := d.take(n)
		if err != nil {
			return 0, 0, err
		}
		for _, x := range bs {
			arg = arg<<8 | uint64(x)
		}
		return major, arg, nil
	}
	return 0, 0, errCBOR // reserved values and indefinite lengths
}

func (d *decoder) item(depth int) (any, error) {
	if depth > maxDepth {
		return nil, errCBOR
	}
	major, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		if arg > math.MaxInt64 {
			return nil, errCBOR
		}
		return int64(arg), nil
	case 1:
		if arg > math.MaxInt64 {
			return nil, errCBOR
		}
		return -1 - int64(arg), nil
	case 2, 3:
		if arg > uint64(len(d.b)) {
			return nil, errCBOR
		}
		bs, err := d.take(int(arg))
		if err != nil {
			return nil, err
		}
		if major == 3 {
			return string(bs), nil
		}
		return append([]byte(nil), bs...), nil
	case 4:
		if arg > maxItems || arg > uint64(len(d.b)) {
			return nil, errCBOR
		}
		out := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		if arg > maxItems || arg > uint64(len(d.b)) {
			return nil, errCBOR
		}
		out := make(map[any]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, errCBOR
			}
			v, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			if _, dup := out[k]; dup {
				return nil, errCBOR
			}
			out[k] = v
		}
		return out, nil
	case 6:
		return nil, errCBOR // tags are not used by WebAuthn
	}
	switch arg { // major 7
	case 20:
		return false, nil
	case 21:
		return true, nil
	case 22, 23:
		return nil, nil
	}
	return nil, errCBOR
}
