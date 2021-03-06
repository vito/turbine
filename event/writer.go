package event

import (
	"io"
	"unicode/utf8"
)

type writer struct {
	emitter Emitter
	origin  Origin

	dangling []byte
}

func NewWriter(emitter Emitter, origin Origin) io.Writer {
	return &writer{
		emitter: emitter,
		origin:  origin,
	}
}

func (writer *writer) Write(data []byte) (int, error) {
	text := append(writer.dangling, data...)

	checkEncoding, _ := utf8.DecodeLastRune(text)
	if checkEncoding == utf8.RuneError {
		writer.dangling = text
		return len(data), nil
	}

	writer.dangling = nil

	writer.emitter.EmitEvent(Log{
		Payload: string(text),
		Origin:  writer.origin,
	})

	return len(data), nil
}
