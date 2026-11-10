package parser

import (
	"bytes"
	"testing"
)

func BenchmarkStreamMatcherSmallReads(b *testing.B) {
	line := []byte("\x1b[2C/remote-control\x1b[1Cis\x1b[1Cactive\x1b[1C· at \x1b[38;5;246msome text\x1b[39m\r\n")
	stream := bytes.Repeat(line, 200)
	s := NewStreamMatcher(claudeSessionRE)
	b.SetBytes(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := (i * 10) % (len(stream) - 10)
		s.Feed(stream[off : off+10])
	}
}

func BenchmarkStreamMatcherLargeReads(b *testing.B) {
	line := []byte("\x1b[2C/remote-control\x1b[1Cis\x1b[1Cactive\x1b[1C· at \x1b[38;5;246msome text\x1b[39m\r\n")
	chunk := bytes.Repeat(line, 600)[:32<<10]
	s := NewStreamMatcher(claudeSessionRE)
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Feed(chunk)
	}
}
