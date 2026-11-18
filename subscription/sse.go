package subscription

import (
	"bufio"
	"bytes"
	"context"
	"io"
)

// visitNativeSSE visits each complete event in an SSE stream. The data slice is
// reused and is valid only until emit returns. The caller owns the reader and
// any cancellation mechanism needed to unblock its Read method.
func visitNativeSSE(ctx context.Context, r io.Reader, emit func(event string, data []byte) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	reader := bufio.NewReader(r)
	parser := nativeSSEParser{
		ctx:       ctx,
		emit:      emit,
		firstLine: true,
	}

	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) != 0 {
			keepGoing, err := parser.consume(chunk)
			if err != nil {
				return err
			}
			if !keepGoing {
				return nil
			}
		}

		if readErr == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr == io.EOF {
			// SSE requires a blank line to dispatch an event. Pending data at
			// EOF is deliberately discarded rather than treated as success.
			return nil
		}
		return readErr
	}
}

type nativeSSEParser struct {
	ctx       context.Context
	emit      func(event string, data []byte) (bool, error)
	line      []byte
	data      []byte
	event     []byte
	firstLine bool
	skipLF    bool
}

func (p *nativeSSEParser) consume(chunk []byte) (bool, error) {
	if err := p.ctx.Err(); err != nil {
		return false, err
	}

	lineStart := 0
	for i, b := range chunk {
		if p.skipLF {
			p.skipLF = false
			if b == '\n' {
				lineStart = i + 1
				continue
			}
		}
		if b != '\r' && b != '\n' {
			continue
		}

		var line []byte
		if len(p.line) == 0 {
			line = chunk[lineStart:i]
		} else {
			p.line = append(p.line, chunk[lineStart:i]...)
			line = p.line
		}

		keepGoing, err := p.processLine(line)
		p.line = p.line[:0]
		if err != nil || !keepGoing {
			return keepGoing, err
		}

		lineStart = i + 1
		p.skipLF = b == '\r'
	}

	if lineStart < len(chunk) {
		p.line = append(p.line, chunk[lineStart:]...)
	}
	return true, nil
}

func (p *nativeSSEParser) processLine(line []byte) (bool, error) {
	if err := p.ctx.Err(); err != nil {
		return false, err
	}

	if p.firstLine {
		p.firstLine = false
		if len(line) >= 3 && line[0] == 0xef && line[1] == 0xbb && line[2] == 0xbf {
			line = line[3:]
		}
	}

	if len(line) == 0 {
		return p.dispatch()
	}
	if line[0] == ':' {
		return true, nil
	}

	field := line
	var value []byte
	if colon := bytes.IndexByte(line, ':'); colon >= 0 {
		field = line[:colon]
		value = line[colon+1:]
		if len(value) != 0 && value[0] == ' ' {
			value = value[1:]
		}
	}

	switch {
	case bytes.Equal(field, []byte("data")):
		p.data = append(p.data, value...)
		p.data = append(p.data, '\n')
	case bytes.Equal(field, []byte("event")):
		p.event = append(p.event[:0], value...)
	}
	return true, nil
}

func (p *nativeSSEParser) dispatch() (bool, error) {
	if len(p.data) == 0 {
		p.event = p.event[:0]
		return true, nil
	}

	event := "message"
	if len(p.event) != 0 {
		event = string(p.event)
	}
	data := p.data[:len(p.data)-1]
	keepGoing, err := p.emit(event, data)
	p.data = p.data[:0]
	p.event = p.event[:0]
	if err != nil {
		return false, err
	}
	return keepGoing, nil
}
