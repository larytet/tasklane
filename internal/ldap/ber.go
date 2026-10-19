// Package ldap is the small part of LDAP v3 (RFC 4511) that a sign-in needs: simple bind, StartTLS and a subtree search, over
// a connection that goes through the egress policy. It is not a general client: no paging, no referrals, no SASL.
package ldap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// BER classes and the universal tags LDAP uses.
const (
	ClassUniversal   = 0x00
	ClassApplication = 0x40
	ClassContext     = 0x80

	TagBoolean     = 1
	TagInteger     = 2
	TagOctetString = 4
	TagEnumerated  = 10
	TagSequence    = 16
	TagSet         = 17
)

const (
	maxMessage = 1 << 20 // a sign-in never needs a larger message; a server that sends one is refused
	maxDepth   = 32
)

var errBER = errors.New("ldap: malformed message")

// Packet is one BER element: a primitive carries Value, a constructed one carries Children.
type Packet struct {
	Class       byte
	Constructed bool
	Tag         int
	Value       []byte
	Children    []*Packet
}

func Seq(class byte, tag int, kids ...*Packet) *Packet {
	return &Packet{Class: class, Constructed: true, Tag: tag, Children: kids}
}

func Str(class byte, tag int, s string) *Packet {
	return &Packet{Class: class, Tag: tag, Value: []byte(s)}
}

func Int(tag int, n int64) *Packet {
	var b []byte
	for {
		b = append([]byte{byte(n)}, b...)
		if (n >= -128 && n < 128) || len(b) == 8 {
			break
		}
		n >>= 8
	}
	return &Packet{Class: ClassUniversal, Tag: tag, Value: b}
}

func Bool(b bool) *Packet {
	v := byte(0)
	if b {
		v = 0xff
	}
	return &Packet{Class: ClassUniversal, Tag: TagBoolean, Value: []byte{v}}
}

// IntValue reads an INTEGER or ENUMERATED.
func (p *Packet) IntValue() (int64, error) {
	if p.Constructed || len(p.Value) == 0 || len(p.Value) > 8 {
		return 0, errBER
	}
	n := int64(int8(p.Value[0]))
	for _, b := range p.Value[1:] {
		n = n<<8 | int64(b)
	}
	return n, nil
}

func (p *Packet) Is(class byte, tag int) bool { return p.Class == class && p.Tag == tag }

func (p *Packet) Encode() []byte {
	var body []byte
	if p.Constructed {
		for _, c := range p.Children {
			body = append(body, c.Encode()...)
		}
	} else {
		body = p.Value
	}
	id := p.Class | byte(p.Tag)
	if p.Constructed {
		id |= 0x20
	}
	out := []byte{id}
	if n := len(body); n < 0x80 {
		out = append(out, byte(n))
	} else {
		var l []byte
		for ; n > 0; n >>= 8 {
			l = append([]byte{byte(n)}, l...)
		}
		out = append(append(out, 0x80|byte(len(l))), l...)
	}
	return append(out, body...)
}

// ReadPacket reads one whole element from the stream.
func ReadPacket(r *bufio.Reader) (*Packet, error) {
	id, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	n, err := readLength(r)
	if err != nil {
		return nil, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return decode(id, body, 0)
}

func readLength(r *bufio.Reader) (int, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	if b < 0x80 {
		return int(b), nil
	}
	k := int(b & 0x7f)
	if k == 0 || k > 4 { // the indefinite form is not allowed in LDAP
		return 0, errBER
	}
	n := 0
	for range k {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		n = n<<8 | int(c)
	}
	if n > maxMessage {
		return 0, fmt.Errorf("%w: a %d byte message is larger than allowed", errBER, n)
	}
	return n, nil
}

func decode(id byte, body []byte, depth int) (*Packet, error) {
	if id&0x1f == 0x1f || depth > maxDepth { // high tag numbers are not used by LDAP
		return nil, errBER
	}
	p := &Packet{Class: id & 0xc0, Constructed: id&0x20 != 0, Tag: int(id & 0x1f)}
	if !p.Constructed {
		p.Value = body
		return p, nil
	}
	for len(body) > 0 {
		if len(body) < 2 {
			return nil, errBER
		}
		cid := body[0]
		n, hdr := int(body[1]), 2
		if body[1] >= 0x80 {
			k := int(body[1] & 0x7f)
			if k == 0 || k > 4 || len(body) < 2+k {
				return nil, errBER
			}
			n = 0
			for _, c := range body[2 : 2+k] {
				n = n<<8 | int(c)
			}
			hdr += k
		}
		if n < 0 || len(body) < hdr+n {
			return nil, errBER
		}
		c, err := decode(cid, body[hdr:hdr+n], depth+1)
		if err != nil {
			return nil, err
		}
		p.Children = append(p.Children, c)
		body = body[hdr+n:]
	}
	return p, nil
}
