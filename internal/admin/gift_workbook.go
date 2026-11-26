package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"iter"

	"github.com/xuri/excelize/v2"
)

const maxGiftWorkbook = 2 << 20
const maxGiftWorkbookExpanded = 64 << 20

var errInvalidGiftWorkbook = errors.New("invalid Excel workbook")
var errMissingGiftSheet = errors.New("missing Sheet1")
var compoundDocumentSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

func openGiftSheet(ctx context.Context, src io.Reader, legacyReader string) (iter.Seq2[[]string, error], bool, func(), error) {
	raw, err := io.ReadAll(io.LimitReader(src, maxGiftWorkbook+1))
	if err != nil || len(raw) > maxGiftWorkbook {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	data := bytes.NewReader(raw)
	if bytes.HasPrefix(raw, compoundDocumentSignature) {
		return openGiftXLS(data)
	}
	archive, err := zip.NewReader(data, int64(len(raw)))
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	var total uint64
	for _, entry := range archive.File {
		if entry.UncompressedSize64 > maxGiftWorkbookExpanded-total {
			return nil, false, nil, errInvalidGiftWorkbook
		}
		total += entry.UncompressedSize64
	}
	var binary, openDocument, hasMimetype bool
	for _, entry := range archive.File {
		switch entry.Name {
		case "xl/workbook.bin":
			binary = true
		case "content.xml":
			openDocument = true
		case "mimetype":
			hasMimetype = true
		}
	}
	if binary {
		return openGiftXLSB(ctx, legacyReader, raw)
	}
	if openDocument && hasMimetype {
		return openGiftODS(archive)
	}
	return openGiftXLSX(data)
}

func openGiftXLSX(src io.Reader) (iter.Seq2[[]string, error], bool, func(), error) {
	book, err := excelize.OpenReader(src, excelize.Options{RawCellValue: true, UnzipSizeLimit: maxGiftWorkbookExpanded, UnzipXMLSizeLimit: 2 << 20})
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	rows, err := book.Rows("Sheet1")
	if err != nil {
		book.Close()
		return nil, false, nil, errMissingGiftSheet
	}
	closeBook := func() { rows.Close(); book.Close() }
	props, err := book.GetWorkbookProps()
	if err != nil {
		closeBook()
		return nil, false, nil, errInvalidGiftWorkbook
	}
	return func(yield func([]string, error) bool) {
		for rows.Next() {
			row, err := rows.Columns(excelize.Options{RawCellValue: true})
			if !yield(row, err) || err != nil {
				return
			}
		}
		if err := rows.Error(); err != nil {
			yield(nil, err)
		}
	}, props.Date1904 != nil && *props.Date1904, closeBook, nil
}
