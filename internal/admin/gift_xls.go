package admin

import (
	"bytes"
	"encoding/binary"
	"io"
	"iter"

	"github.com/asalih/go-mscfb"
	xlsreader "github.com/sergrom/xls-reader"
)

func openGiftXLS(src *bytes.Reader) (iter.Seq2[[]string, error], bool, func(), error) {
	book, err := xlsreader.Parse(src)
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	sheet, found := book.SheetByName("Sheet1")
	if !found || sheet.Name != "Sheet1" {
		return nil, false, nil, errMissingGiftSheet
	}
	date1904, err := giftXLSDateMode(src)
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	return func(yield func([]string, error) bool) {
		for _, row := range sheet.Rows() {
			if !yield(row, nil) {
				return
			}
		}
	}, date1904, func() {}, nil
}

// The XLS reader preserves raw numeric and cached formula values. Read the
// workbook-level BIFF DATEMODE record to interpret numeric dates correctly.
func giftXLSDateMode(src *bytes.Reader) (bool, error) {
	container, err := mscfb.Open(src, mscfb.ValidationPermissive)
	if err != nil {
		return false, err
	}
	stream, err := container.OpenStream("Workbook")
	if err != nil {
		stream, err = container.OpenStream("Book")
	}
	if err != nil {
		return false, err
	}
	input := io.LimitReader(stream, int64(src.Size()))
	for {
		var header [4]byte
		if _, err := io.ReadFull(input, header[:]); err != nil {
			return false, err
		}
		record, length := binary.LittleEndian.Uint16(header[:2]), binary.LittleEndian.Uint16(header[2:])
		switch record {
		case 0x0022: // DATEMODE
			var mode uint16
			if length != 2 {
				return false, errInvalidGiftWorkbook
			}
			if err := binary.Read(input, binary.LittleEndian, &mode); err != nil {
				return false, err
			}
			if mode > 1 {
				return false, errInvalidGiftWorkbook
			}
			return mode == 1, nil
		case 0x000a: // End of the workbook globals.
			return false, nil
		default:
			if _, err := io.CopyN(io.Discard, input, int64(length)); err != nil {
				return false, err
			}
		}
	}
}
