package admin

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"io"
	"iter"
	"strconv"
	"strings"
)

const odsTableNamespace = "urn:oasis:names:tc:opendocument:xmlns:table:1.0"
const odsTextNamespace = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"

type odsGiftRow struct {
	Repeat int           `xml:"number-rows-repeated,attr"`
	Cells  []odsGiftCell `xml:",any"`
}

type odsGiftCell struct {
	XMLName    xml.Name
	Type       string        `xml:"value-type,attr"`
	Value      string        `xml:"value,attr"`
	Text       *string       `xml:"string-value,attr"`
	Date       string        `xml:"date-value,attr"`
	Repeat     int           `xml:"number-columns-repeated,attr"`
	Paragraphs []odsGiftText `xml:"p"`
}

type odsGiftText string

func (text *odsGiftText) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	var content strings.Builder
	for depth := 1; depth > 0; {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if value.Name.Space != odsTextNamespace {
				continue
			}
			// Formatted text can contain spans, tabs, and nonbreaking spaces.
			switch value.Name.Local {
			case "s":
				count := 1
				for _, attr := range value.Attr {
					if attr.Name.Space == odsTextNamespace && attr.Name.Local == "c" {
						count, err = strconv.Atoi(attr.Value)
						if err != nil || count < 1 || count > 32_767 {
							return errInvalidGiftWorkbook
						}
					}
				}
				content.WriteString(strings.Repeat(" ", count))
			case "tab":
				content.WriteByte('\t')
			case "line-break":
				content.WriteByte('\n')
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			content.Write(value)
		}
		if content.Len() > maxGiftWorkbook {
			return errInvalidGiftWorkbook
		}
	}
	*text = odsGiftText(content.String())
	return nil
}

func openGiftODS(archive *zip.Reader) (iter.Seq2[[]string, error], bool, func(), error) {
	kind, err := archive.Open("mimetype")
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	mime, err := io.ReadAll(io.LimitReader(kind, 128))
	kind.Close()
	if err != nil || string(mime) != "application/vnd.oasis.opendocument.spreadsheet" {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	content, err := archive.Open("content.xml")
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	return func(yield func([]string, error) bool) {
		decoder := xml.NewDecoder(io.LimitReader(content, maxGiftWorkbookExpanded+1))
		inSheet := false
		maxRows := 1_048_576
		for {
			token, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				yield(nil, errMissingGiftSheet)
				return
			}
			if err != nil {
				yield(nil, errInvalidGiftWorkbook)
				return
			}
			switch value := token.(type) {
			case xml.StartElement:
				if value.Name.Space != odsTableNamespace {
					continue
				}
				if value.Name.Local == "table" {
					if inSheet {
						if err := decoder.Skip(); err != nil {
							yield(nil, errInvalidGiftWorkbook)
							return
						}
						continue
					}
					for _, a := range value.Attr {
						if a.Name.Space == odsTableNamespace && a.Name.Local == "name" && a.Value == "Sheet1" {
							inSheet = true
						}
					}
					if !inSheet {
						if err := decoder.Skip(); err != nil {
							yield(nil, errInvalidGiftWorkbook)
							return
						}
					}
				}
				if !inSheet || value.Name.Local != "table-row" {
					continue
				}
				var raw odsGiftRow
				if err := decoder.DecodeElement(&raw, &value); err != nil {
					yield(nil, errInvalidGiftWorkbook)
					return
				}
				row := raw.values()
				repeat := max(1, raw.Repeat)
				if repeat > maxRows {
					yield(nil, errInvalidGiftWorkbook)
					return
				}
				maxRows -= repeat
				for i := 0; i < repeat; i++ {
					if !yield(row, nil) {
						return
					}
				}
			case xml.EndElement:
				if inSheet && value.Name.Space == odsTableNamespace && value.Name.Local == "table" {
					return
				}
			}
		}
	}, false, func() { content.Close() }, nil
}

func (row odsGiftRow) values() []string {
	// The widest supported template has 28 columns. Expanded trailing blanks
	// and unused columns don't belong to the import.
	const maxCols = 28
	var values []string
	for _, cell := range row.Cells {
		if len(values) >= maxCols {
			break
		}
		var value string
		if cell.XMLName.Local == "table-cell" {
			switch {
			case cell.Type == "date":
				value = cell.Date
			case cell.Value != "":
				value = cell.Value
			case cell.Text != nil:
				value = *cell.Text
			default:
				lines := make([]string, len(cell.Paragraphs))
				for i, line := range cell.Paragraphs {
					lines[i] = string(line)
				}
				value = strings.Join(lines, "\n")
			}
		}
		repeat := min(max(1, cell.Repeat), maxCols-len(values))
		for i := 0; i < repeat; i++ {
			values = append(values, value)
		}
	}
	for len(values) > 0 && values[len(values)-1] == "" {
		values = values[:len(values)-1]
	}
	return values
}
