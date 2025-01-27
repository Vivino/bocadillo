package binlog

import (
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/Vivino/bocadillo/buffer"
	"github.com/Vivino/bocadillo/mysql"
)

// RowsEvent contains a Rows Event.
type RowsEvent struct {
	Type          EventType
	TableID       uint64
	Flags         RowsFlag
	ExtraData     []byte
	ColumnCount   uint64
	ColumnBitmap1 []byte
	ColumnBitmap2 []byte
	Rows          [][]interface{}
}

// RowsFlag is bitmask of flags.
type RowsFlag uint16

const (
	// RowsFlagEndOfStatement is used to clear old table mappings.
	RowsFlagEndOfStatement RowsFlag = 0x0001
)

// PeekTableIDAndFlags returns table ID and flags without decoding whole event.
func (e *RowsEvent) PeekTableIDAndFlags(connBuff []byte, fd FormatDescription) (uint64, RowsFlag) {
	if fd.TableIDSize(e.Type) == 6 {
		return mysql.DecodeUint48(connBuff), RowsFlag(mysql.DecodeUint16(connBuff[6:]))
	}
	return uint64(mysql.DecodeUint32(connBuff)), RowsFlag(mysql.DecodeUint16(connBuff[4:]))
}

// Decode decodes given buffer into a rows event event.
func (e *RowsEvent) Decode(connBuff []byte, fd FormatDescription, td TableDescription) (err error) {
	defer func() {
		if errv := recover(); errv != nil {
			fmt.Println("Recovered from panic in RowsEvent.Decode")
			fmt.Println("Error:", errv)
			fmt.Println("Format:", fd)
			fmt.Println("Table:", td)
			fmt.Println("Columns:")
			for _, ctb := range td.ColumnTypes {
				fmt.Println(" ", mysql.ColumnType(ctb).String())
			}
			fmt.Println("\nBuffer:")
			fmt.Println(hex.Dump(connBuff))
			fmt.Println("Stacktrace:")
			debug.PrintStack()
			err = errors.New(fmt.Sprint(errv))
		}
	}()

	buf := buffer.New(connBuff)
	idSize := fd.TableIDSize(e.Type)
	if idSize == 6 {
		e.TableID = buf.ReadUint48()
	} else {
		e.TableID = uint64(buf.ReadUint32())
	}

	e.Flags = RowsFlag(buf.ReadUint16())

	if RowsEventHasExtraData(e.Type) {
		// Extra data length is part of extra data, deduct 2 bytes as they
		// already store its length
		extraLen := buf.ReadUint16() - 2
		e.ExtraData = buf.ReadStringVarLen(int(extraLen))
	}

	e.ColumnCount, _, _ = buf.ReadUintLenEnc()
	e.ColumnBitmap1 = buf.ReadStringVarLen(int(e.ColumnCount+7) / 8)
	if RowsEventHasSecondBitmap(e.Type) {
		e.ColumnBitmap2 = buf.ReadStringVarLen(int(e.ColumnCount+7) / 8)
	}

	e.Rows = make([][]interface{}, 0)
	for {
		row, err := e.decodeRows(buf, td, e.ColumnBitmap1)
		if err != nil {
			return err
		}
		e.Rows = append(e.Rows, row)

		if RowsEventHasSecondBitmap(e.Type) {
			row, err := e.decodeRows(buf, td, e.ColumnBitmap2)
			if err != nil {
				return err
			}
			e.Rows = append(e.Rows, row)
		}
		if !buf.More() {
			break
		}
	}
	return nil
}

func (e *RowsEvent) decodeRows(buf *buffer.Buffer, td TableDescription, bm []byte) ([]interface{}, error) {
	count := 0
	for i := 0; i < int(e.ColumnCount); i++ {
		if isBitSet(bm, i) {
			count++
		}
	}
	count = (count + 7) / 8

	nullBM := buf.ReadStringVarLen(count)
	nullIdx := 0
	row := make([]interface{}, e.ColumnCount)
	for i := 0; i < int(e.ColumnCount); i++ {
		if !isBitSet(bm, i) {
			continue
		}

		isNull := (uint32(nullBM[nullIdx/8]) >> uint32(nullIdx%8)) & 1
		nullIdx++
		if isNull > 0 {
			row[i] = nil
			continue
		}

		row[i] = e.decodeValue(buf, mysql.ColumnType(td.ColumnTypes[i]), td.ColumnMeta[i])
	}
	return row, nil
}

func (e *RowsEvent) decodeValue(buf *buffer.Buffer, ct mysql.ColumnType, meta uint16) interface{} {
	var length int
	if ct == mysql.ColumnTypeString {
		if meta > 0xFF {
			typeByte := uint8(meta >> 8)
			lengthByte := uint8(meta & 0xFF)
			if typeByte&0x30 != 0x30 {
				ct = mysql.ColumnType(typeByte | 0x30)
				length = int(uint16(lengthByte) | (uint16((typeByte&0x30)^0x30) << 4))
			} else {
				ct = mysql.ColumnType(typeByte)
				length = int(lengthByte)
			}
		}
	}

	switch ct {
	case mysql.ColumnTypeNull:
		return nil

	// Integer
	case mysql.ColumnTypeTiny:
		return buf.ReadUint8()
	case mysql.ColumnTypeShort:
		return buf.ReadUint16()
	case mysql.ColumnTypeInt24:
		return buf.ReadUint24()
	case mysql.ColumnTypeLong:
		return buf.ReadUint32()
	case mysql.ColumnTypeLonglong:
		return buf.ReadUint64()

	// Float
	case mysql.ColumnTypeFloat:
		return buf.ReadFloat32()
	case mysql.ColumnTypeDouble:
		return buf.ReadFloat64()

	// Decimals
	case mysql.ColumnTypeNewDecimal:
		precision := int(meta >> 8)
		decimals := int(meta & 0xFF)
		return buf.ReadDecimal(precision, decimals)

	// Date and Time
	case mysql.ColumnTypeYear:
		return mysql.DecodeYear(buf.ReadUint8())
	case mysql.ColumnTypeDate:
		return mysql.DecodeDate(buf.ReadUint24())
	case mysql.ColumnTypeTime:
		return mysql.DecodeTime(buf.ReadUint24())
	case mysql.ColumnTypeTime2:
		v, n := mysql.DecodeTime2(buf.Cur(), meta)
		buf.Skip(n)
		return v
	case mysql.ColumnTypeTimestamp:
		v, n := mysql.DecodeTimestamp(buf.Cur(), meta)
		buf.Skip(n)
		return v
	case mysql.ColumnTypeTimestamp2:
		v, n := mysql.DecodeTimestamp2(buf.Cur(), meta)
		buf.Skip(n)
		return v
	case mysql.ColumnTypeDatetime:
		return mysql.DecodeDatetime(buf.ReadUint64())
	case mysql.ColumnTypeDatetime2:
		v, n := mysql.DecodeDatetime2(buf.Cur(), meta)
		buf.Skip(n)
		return v

	// Strings
	case mysql.ColumnTypeString:
		return readString(buf, length)
	case mysql.ColumnTypeVarchar, mysql.ColumnTypeVarstring:
		return readString(buf, int(meta))

	// Blobs
	case mysql.ColumnTypeBlob, mysql.ColumnTypeGeometry:
		return buf.ReadStringVarEnc(int(meta))
	case mysql.ColumnTypeJSON:
		jdata := buf.ReadStringVarEnc(int(meta))
		rawj, _ := mysql.DecodeJSON(jdata)
		return rawj
	case mysql.ColumnTypeTinyblob:
		return buf.ReadStringVarEnc(1)
	case mysql.ColumnTypeMediumblob:
		return buf.ReadStringVarEnc(3)
	case mysql.ColumnTypeLongblob:
		return buf.ReadStringVarEnc(4)

	// Other
	case mysql.ColumnTypeBit:
		nbits := int(((meta >> 8) * 8) + (meta & 0xFF))
		length = int(nbits+7) / 8
		v, n := mysql.DecodeBit(buf.Cur(), nbits, length)
		buf.Skip(n)
		return v
	case mysql.ColumnTypeSet:
		nbits := length * 8
		v, n := mysql.DecodeBit(buf.Cur(), nbits, length)
		buf.Skip(n)
		return v
	case mysql.ColumnTypeEnum:
		return buf.ReadVarLen64(length)

	// Unsupported
	case mysql.ColumnTypeDecimal:
		// Old decimal
		fallthrough
	case mysql.ColumnTypeNewDate:
		// Too new
		fallthrough
	default:
		return fmt.Errorf("unsupported type: %d (%s) %x %x", ct, ct.String(), meta, buf.Cur())
	}
}

func readString(buf *buffer.Buffer, length int) string {
	// Length is encoded in 1 byte
	if length < 256 {
		return string(buf.ReadStringVarEnc(1))
	}
	// Length is encoded in 2 bytes
	return string(buf.ReadStringVarEnc(2))
}

func isBitSet(bm []byte, i int) bool {
	return bm[i>>3]&(1<<(uint(i)&7)) > 0
}

// RowsEventVersion returns rows event versions. If event is not a rows type -1
// is returned.
func RowsEventVersion(et EventType) int {
	switch et {
	case EventTypeWriteRowsV0, EventTypeUpdateRowsV0, EventTypeDeleteRowsV0:
		return 0
	case EventTypeWriteRowsV1, EventTypeUpdateRowsV1, EventTypeDeleteRowsV1:
		return 1
	case EventTypeWriteRowsV2, EventTypeUpdateRowsV2, EventTypeDeleteRowsV2:
		return 2
	default:
		return -1
	}
}

// RowsEventHasExtraData returns true if given event is of rows type and
// contains extra data.
func RowsEventHasExtraData(et EventType) bool {
	return RowsEventVersion(et) == 2
}

// RowsEventHasSecondBitmap returns true if given event is of rows type and
// contains a second bitmap.
func RowsEventHasSecondBitmap(et EventType) bool {
	switch et {
	case EventTypeUpdateRowsV1, EventTypeUpdateRowsV2:
		return true
	default:
		return false
	}
}
