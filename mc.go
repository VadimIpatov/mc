package mc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
)

// Errors
var (
	ErrNotFound       = os.NewError("mc: not found")
	ErrKeyExists      = os.NewError("mc: key exists")
	ErrValueTooLarge  = os.NewError("mc: value to large")
	ErrInvalidArgs    = os.NewError("mc: invalid arguments")
	ErrValueNotStored = os.NewError("mc: value not stored")
	ErrNonNumeric     = os.NewError("mc: incr/decr called on non-numeric value")
	ErrAuthRequired   = os.NewError("mc: authentication required")
	ErrUnknownCommand = os.NewError("mc: unknown command")
	ErrOutOfMemory    = os.NewError("mc: out of memory")
)

var errMap = map[uint16]os.Error{
	0:    nil,
	1:    ErrNotFound,
	2:    ErrKeyExists,
	3:    ErrValueTooLarge,
	4:    ErrInvalidArgs,
	5:    ErrValueNotStored,
	6:    ErrNonNumeric,
	0x20: ErrAuthRequired,
	0x81: ErrUnknownCommand,
	0x82: ErrOutOfMemory,
}

// Ops
const (
	OpGet = uint8(iota)
	OpSet
	OpAdd
	OpReplace
	OpDelete
	OpIncrement
	OpDecrement
	OpQuit
	OpFlush
	OpGetQ
	OpNoop
	OpVersion
	OpGetK
	OpGetKQ
	OpAppend
	OpPrepend
	OpStat
	OpSetQ
	OpAddQ
	OpReplaceQ
	OpDeleteQ
	OpIncrementQ
	OpDecrementQ
	OpQuitQ
	OpFlushQ
	OpAppendQ
	OpPrependQ
)

// Auth Ops
const (
	OpAuthList = uint8(iota + 0x20)
	OpAuthStart
	OpAuthStep
)

type header struct {
	Magic        uint8
	Op           uint8
	KeyLen       uint16
	ExtraLen     uint8
	DataType     uint8
	ResvOrStatus uint16
	BodyLen      uint32
	Opaque       uint32
	CAS          uint64
}

type body struct {
	iextras []interface{}
	oextras []interface{}
	key     string
	val     string
}

type Conn struct {
	rwc io.ReadWriteCloser
	l   sync.Mutex
	buf *bytes.Buffer
}

func Dial(nett, addr string) (*Conn, os.Error) {
	nc, err := net.Dial(nett, addr)
	if err != nil {
		return nil, err
	}

	cn := &Conn{rwc: nc, buf: new(bytes.Buffer)}
	return cn, nil
}

func (cn *Conn) Get(key string) (val string, cas int, err os.Error) {
	h := &header{
		Op: OpGet,
	}

	b := &body{
		key: key,
	}

	err = cn.send(h, b)

	return b.val, int(h.CAS), err
}

func (cn *Conn) Set(key, val string, ocas, flags, exp int) os.Error {
	h := &header{
		Op:  OpSet,
		CAS: uint64(ocas),
	}

	b := &body{
		iextras: []interface{}{uint32(flags), uint32(exp)},
		key:     key,
		val:     val,
	}

	return cn.send(h, b)
}

func (cn *Conn) Del(key string) os.Error {
	h := &header{
		Op: OpDelete,
	}

	b := &body{
		key: key,
	}

	return cn.send(h, b)
}

func (cn *Conn) Incr(key string, delta, init, exp int) (n, cas int, err os.Error) {
	return cn.incrdecr(OpIncrement, key, delta, init, exp)
}

func (cn *Conn) Decr(key string, delta, init, exp int) (n, cas int, err os.Error) {
	return cn.incrdecr(OpDecrement, key, delta, init, exp)
}

func (cn *Conn) Auth(user, pass string) os.Error {
	s, err := cn.authList()
	if err != nil {
		return err
	}

	switch {
	case strings.Index(s, "PLAIN") != -1:
		return cn.authPlain(user, pass)
	}

	return fmt.Errorf("mc: unknown auth types %q", s)
}

func (cn *Conn) authList() (s string, err os.Error) {
	h := &header{
		Op: OpAuthList,
	}

	b := &body{}

	err = cn.send(h, b)
	return b.val, err
}

func (cn *Conn) authPlain(user, pass string) os.Error {
	h := &header{
		Op: OpAuthStart,
	}

	b := &body{
		key: "PLAIN",
		val: fmt.Sprintf("\x00%s\x00%s", user, pass),
	}

	return cn.send(h, b)
}

func (cn *Conn) incrdecr(op uint8, key string, delta, init, exp int) (n, cas int, err os.Error) {
	h := &header{
		Op: op,
	}

	b := &body{
		key:     key,
		iextras: []interface{}{uint64(delta), uint64(delta), uint32(exp)},
	}

	err = cn.send(h, b)
	if err != nil {
		return
	}

	return readInt(b.val), int(h.CAS), nil
}

func (cn *Conn) send(h *header, b *body) (err os.Error) {
	const magic uint8 = 0x80

	h.Magic = magic
	h.ExtraLen = sizeOfExtras(b.iextras)
	h.KeyLen = uint16(len(b.key))
	h.BodyLen = uint32(h.ExtraLen) + uint32(h.KeyLen) + uint32(len(b.val))

	cn.l.Lock()
	defer cn.l.Unlock()

	// Request
	err = binary.Write(cn.buf, binary.BigEndian, h)
	if err != nil {
		return
	}

	for _, e := range b.iextras {
		err = binary.Write(cn.buf, binary.BigEndian, e)
		if err != nil {
			return
		}
	}

	_, err = io.WriteString(cn.buf, b.key)
	if err != nil {
		return
	}

	_, err = io.WriteString(cn.buf, b.val)
	if err != nil {
		return
	}

	cn.buf.WriteTo(cn.rwc)

	// Response
	err = binary.Read(cn.rwc, binary.BigEndian, h)
	if err != nil {
		return err
	}

	bd := make([]byte, h.BodyLen)
	_, err = io.ReadFull(cn.rwc, bd)
	if err != nil {
		return err
	}

	buf := bytes.NewBuffer(bd)

	for _, e := range b.oextras {
		err = binary.Read(buf, binary.BigEndian, e)
		if err != nil {
			return
		}
	}

	b.key = string(buf.Next(int(h.KeyLen)))

	vlen := int(h.BodyLen) - int(h.ExtraLen) - int(h.KeyLen)
	b.val = string(buf.Next(int(vlen)))

	return checkError(h)
}

func checkError(h *header) os.Error {
	err, ok := errMap[h.ResvOrStatus]
	if !ok {
		fmt.Printf("status: %d\n", h.ResvOrStatus)
		return os.NewError("mc: unknown error from server")
	}
	return err
}

func sizeOfExtras(extras []interface{}) (l uint8) {
	for _, e := range extras {
		switch e.(type) {
		default:
			panic(fmt.Sprintf("mc: unknown extra type (%T)", e))
		case uint8:
			l += 8 / 8
		case uint16:
			l += 16 / 8
		case uint32:
			l += 32 / 8
		case uint64:
			l += 64 / 8
		}
	}
	return
}

func readInt(b string) int {
	switch len(b) {
	case 8: // 64 bit
		return int(uint64(b[7]) | uint64(b[6])<<8 | uint64(b[5])<<16 | uint64(b[4])<<24 |
			uint64(b[3])<<32 | uint64(b[2])<<40 | uint64(b[1])<<48 | uint64(b[0])<<56)
	}

	panic(fmt.Sprintf("mc: don't know how to parse string with %d bytes", len(b)))
}
