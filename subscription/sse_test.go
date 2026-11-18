package subscription

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type nativeSSETestFrame struct {
	event string
	data  string
}

func TestVisitNativeSSEFramesFragmentedStream(t *testing.T) {
	stream := "\xef\xbb\xbf: ready\r\n" +
		"id: ignored\r\n" +
		"retry: 250\r\n" +
		"event: delta\r\n" +
		"data: first\r\n" +
		"data:second\r\n" +
		"\r\n" +
		": keepalive\n" +
		"\n" +
		"event: stale\r\r" +
		"event:\r" +
		"data:\r" +
		"\r" +
		"event: done\n" +
		"data:  leading space\n" +
		"ignored: value\n" +
		"\n"
	reader := &nativeSSEFragmentedReader{
		data:  []byte(stream),
		sizes: []int{1, 2, 5, 3, 8},
	}

	var got []nativeSSETestFrame
	err := visitNativeSSE(context.Background(), reader, func(event string, data []byte) (bool, error) {
		got = append(got, nativeSSETestFrame{event: event, data: string(data)})
		return true, nil
	})
	if err != nil {
		t.Fatalf("visitNativeSSE returned %v", err)
	}

	want := []nativeSSETestFrame{
		{event: "delta", data: "first\nsecond"},
		{event: "message", data: ""},
		{event: "done", data: " leading space"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames (%#v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestVisitNativeSSEAcceptsLargeDataLine(t *testing.T) {
	payload := `{"name":"tool","arguments":"` + strings.Repeat("x", 256<<10) + `"}`
	reader := &nativeSSEFragmentedReader{
		data:  []byte("event: tool\ndata: " + payload + "\n\n"),
		sizes: []int{7, 31, 4093, 2},
	}

	calls := 0
	err := visitNativeSSE(context.Background(), reader, func(event string, data []byte) (bool, error) {
		calls++
		if event != "tool" {
			t.Errorf("event = %q, want tool", event)
		}
		if len(data) != len(payload) || string(data) != payload {
			t.Errorf("large data mismatch: got %d bytes, want %d", len(data), len(payload))
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("visitNativeSSE returned %v", err)
	}
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
}

func TestVisitNativeSSECallbackControl(t *testing.T) {
	const stream = "data: one\n\ndata: two\n\ndata: three\n\n"

	t.Run("stop", func(t *testing.T) {
		calls := 0
		err := visitNativeSSE(context.Background(), strings.NewReader(stream), func(_ string, _ []byte) (bool, error) {
			calls++
			return calls < 2, nil
		})
		if err != nil {
			t.Fatalf("visitNativeSSE returned %v", err)
		}
		if calls != 2 {
			t.Fatalf("callback calls = %d, want 2", calls)
		}
	})

	t.Run("error", func(t *testing.T) {
		wantErr := errors.New("consumer failed")
		calls := 0
		err := visitNativeSSE(context.Background(), strings.NewReader(stream), func(_ string, _ []byte) (bool, error) {
			calls++
			return true, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})
}

func TestVisitNativeSSEPropagatesReaderError(t *testing.T) {
	wantErr := errors.New("stream read failed")
	reader := &nativeSSEErrorReader{
		data: []byte("event: delta\ndata: accepted\n\nevent: partial\ndata: discarded"),
		err:  wantErr,
	}

	var got []nativeSSETestFrame
	err := visitNativeSSE(context.Background(), reader, func(event string, data []byte) (bool, error) {
		got = append(got, nativeSSETestFrame{event: event, data: string(data)})
		return true, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if len(got) != 1 || got[0] != (nativeSSETestFrame{event: "delta", data: "accepted"}) {
		t.Fatalf("frames = %#v, want only the complete event", got)
	}
}

func TestVisitNativeSSEDiscardsIncompleteEventAtEOF(t *testing.T) {
	calls := 0
	err := visitNativeSSE(context.Background(), strings.NewReader("event: completed\ndata: not-yet-framed"), func(_ string, _ []byte) (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("visitNativeSSE returned %v", err)
	}
	if calls != 0 {
		t.Fatalf("callback calls = %d, want 0", calls)
	}
}

func TestVisitNativeSSEObservesContextCancellation(t *testing.T) {
	t.Run("before read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &nativeSSECountingReader{}

		err := visitNativeSSE(ctx, reader, func(_ string, _ []byte) (bool, error) {
			t.Fatal("callback must not run")
			return true, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if reader.reads != 0 {
			t.Fatalf("reader calls = %d, want 0", reader.reads)
		}
	})

	t.Run("between frames", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := visitNativeSSE(ctx, strings.NewReader("data: first\n\ndata: second\n\n"), func(_ string, data []byte) (bool, error) {
			calls++
			if string(data) != "first" {
				t.Fatalf("data = %q, want first", data)
			}
			cancel()
			return true, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})
}

type nativeSSEFragmentedReader struct {
	data  []byte
	sizes []int
	off   int
	step  int
}

func (r *nativeSSEFragmentedReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if len(r.sizes) != 0 {
		n = r.sizes[r.step%len(r.sizes)]
		r.step++
		if n > len(p) {
			n = len(p)
		}
	}
	if remaining := len(r.data) - r.off; n > remaining {
		n = remaining
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

type nativeSSEErrorReader struct {
	data []byte
	err  error
}

func (r *nativeSSEErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

type nativeSSECountingReader struct {
	reads int
}

func (r *nativeSSECountingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("unexpected read")
}
