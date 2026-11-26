package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

var errXLSBReaderUnavailable = errors.New("XLSB reader is not configured")

// Python is only used when an operator enables XLSB support. Calamine reads
// cached values from the workbook and does not run macros or recalculate cells.
const readGiftXLSBScript = `
import datetime
import io
import json
import math
import sys
try:
    from python_calamine import CalamineWorkbook
except ImportError:
    sys.exit(3)

def stored(value):
    if isinstance(value, (datetime.date, datetime.datetime, datetime.time)):
        return value.isoformat()
    if isinstance(value, bool):
        return "TRUE" if value else "FALSE"
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return str(int(value))
    return str(value) if value is not None else ""

with CalamineWorkbook.from_filelike(io.BytesIO(sys.stdin.buffer.read())) as book:
    if "Sheet1" not in book.sheet_names:
        print("null")
    else:
        sheet = book.get_sheet_by_name("Sheet1")
        if not sheet.height or not sheet.width:
            print("[]")
        elif (sheet.end[0] + 1) * (sheet.end[1] + 1) > 1400000:
            raise ValueError("worksheet is too large")
        else:
            rows = sheet.to_python(skip_empty_area=False)
            print(json.dumps([[stored(v) for v in row[:28]] for row in rows]))
`

func openGiftXLSB(ctx context.Context, python string, raw []byte) (iter.Seq2[[]string, error], bool, func(), error) {
	if python == "" {
		return nil, false, nil, errXLSBReaderUnavailable
	}
	if !filepath.IsAbs(python) {
		return nil, false, nil, errXLSBReaderUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", readGiftXLSBScript)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = []string{}
	for _, key := range []string{"SystemRoot", "WINDIR", "TMP", "TEMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	if err := cmd.Start(); err != nil {
		return nil, false, nil, errXLSBReaderUnavailable
	}
	const outputLimit = 16 << 20
	result, readErr := io.ReadAll(io.LimitReader(output, outputLimit+1))
	if readErr != nil || len(result) > outputLimit {
		cancel()
	}
	waitErr := cmd.Wait()
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) && exit.ExitCode() == 3 {
		return nil, false, nil, errXLSBReaderUnavailable
	}
	if waitErr != nil || readErr != nil || len(result) > outputLimit {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	var rows [][]string
	if json.Unmarshal(result, &rows) != nil {
		return nil, false, nil, errInvalidGiftWorkbook
	}
	if rows == nil {
		return nil, false, nil, errMissingGiftSheet
	}
	return func(yield func([]string, error) bool) {
		for row := range slices.Values(rows) {
			for len(row) > 0 && row[len(row)-1] == "" {
				row = row[:len(row)-1]
			}
			if !yield(row, nil) {
				return
			}
		}
	}, false, func() {}, nil
}
