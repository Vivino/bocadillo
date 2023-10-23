package binlog

import "github.com/Vivino/bocadillo/buffer"

// Position is a pair of log file name and a binary offset in it that is used to
// represent the beginning of the event description.
type Position struct {
	File   string
	Offset uint64
}

// RotateEvent is written at the end of the file that points to the next file in
// the squence. It is written when a binary log file exceeds a size limit.
type RotateEvent struct {
	NextFile Position
}

// Decode decodes given buffer into a rotate event.
// Spec: https://dev.mysql.com/doc/internals/en/rotate-event.html
func (e *RotateEvent) Decode(connBuff []byte, fd FormatDescription) error {
	buf := buffer.New(connBuff)
	if fd.Version > 1 {
		e.NextFile.Offset = buf.ReadUint64()
	} else {
		e.NextFile.Offset = 4
	}
	e.NextFile.File = string(buf.ReadStringEOF())
	return nil
}
