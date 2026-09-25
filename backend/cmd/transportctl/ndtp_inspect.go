package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ebiskauesbi/transport-prediction/backend/internal/ndtp"
)

func hexdump(b []byte, indent string) string {
	var out strings.Builder
	for offset := 0; offset < len(b); offset += 16 {
		end := min(offset+16, len(b))
		fmt.Fprintf(&out, "%s%04X  ", indent, offset)
		for i := offset; i < offset+16; i++ {
			if i < end {
				fmt.Fprintf(&out, "%02X ", b[i])
			} else {
				out.WriteString("   ")
			}
			if i == offset+7 {
				out.WriteString(" ")
			}
		}
		out.WriteString(" |")
		for i := offset; i < end; i++ {
			if b[i] >= 0x20 && b[i] < 0x7F {
				out.WriteByte(b[i])
			} else {
				out.WriteByte('.')
			}
		}
		out.WriteString("|\n")
	}
	return out.String()
}

func describe(frame ndtp.Frame, seq int) frameRecord {
	record := frameRecord{
		Seq:       seq,
		UnitID:    frame.NPL.PeerAddress,
		ServiceID: frame.NPH.ServiceID,
		Type:      frame.NPH.Type,
		Flags:     frame.NPH.Flags,
		RawLen:    len(frame.Raw),
	}
	cells, err := ndtp.ParseCells(frame.Body)
	if err != nil {
		return record
	}
	for _, cell := range cells {
		record.Cells = append(record.Cells, cellRecord{
			Type:    cell.Type().String(),
			Number:  cell.Number,
			Size:    cell.PayloadCellSize(),
			Decoded: cell.Payload,
		})
	}
	return record
}

func inspectFrame(seq int, frame ndtp.Frame) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "=== кадр #%d: %d байт ===\n", seq, len(frame.Raw))
	fmt.Fprintf(&out, "NPL: signature=0x%04X dataSize=%d flags=0x%04X crc=0x%04X type=0x%02X peer=%d requestId=%d\n",
		frame.NPL.Signature, frame.NPL.DataSize, frame.NPL.Flags, frame.NPL.CRC,
		frame.NPL.FrameType, frame.NPL.PeerAddress, frame.NPL.RequestID)
	fmt.Fprintf(&out, "NPH: serviceId=%d type=%d flags=0x%04X requestId=%d\n",
		frame.NPH.ServiceID, frame.NPH.Type, frame.NPH.Flags, frame.NPH.RequestID)

	cells, err := ndtp.ParseCells(frame.Body)
	if err != nil {
		return out.String(), err
	}
	fmt.Fprintf(&out, "ячеек: %d (%d байт тела)\n", len(cells), len(frame.Body))
	for i, cell := range cells {
		decoded, _ := json.Marshal(cell.Payload)
		fmt.Fprintf(&out, "  [%d] %s number=%d size=%d %s\n",
			i, cell.Type(), cell.Number, cell.PayloadCellSize(), decoded)
	}
	return out.String(), nil
}

func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("ndtp-inspect", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "вывести только JSON")
	limit := fs.Int("limit", 5, "сколько кадров показать (0 — все)")
	paths, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	if len(paths) < 1 {
		return errors.New("нужен путь к файлу с пакетами")
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		return err
	}

	reader := ndtp.NewReader(bytes.NewReader(data))
	var rendered []string
	var records []frameRecord
	seq := 0
	for {
		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("кадр #%d: %w", seq, err)
		}
		records = append(records, describe(frame, seq))

		if *limit == 0 || seq < *limit {
			text, err := inspectFrame(seq, frame)
			if err != nil {
				return fmt.Errorf("кадр #%d: %w", seq, err)
			}
			rendered = append(rendered, text+hexdump(frame.Raw, "  "))
		}
		seq++
	}

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(records)
	}
	for _, text := range rendered {
		fmt.Print(text)
	}
	fmt.Printf("всего кадров: %d\n", seq)
	return nil
}
