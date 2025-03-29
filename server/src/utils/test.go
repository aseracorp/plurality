package utils

import (
	"encoding/binary"
	"io"
	"log"
	"os"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

func TestF() {
	// Read raw PCM data from input file.
	in, err := os.Open("audio-before.pcm")
	if err != nil {
		log.Fatal(err)
	}

	// Output file.
	out, err := os.Create("output.wav")
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	// 8 kHz, 16 bit, 1 channel, WAV.
	e := wav.NewEncoder(out, 8000, 16, 1, 1)

	// Create new audio.IntBuffer.
	audioBuf, err := newAudioIntBuffer(in)
	if err != nil {
		log.Fatal(err)
	}
	// Write buffer to output file. This writes a RIFF header and the PCM chunks from the audio.IntBuffer.
	if err := e.Write(audioBuf); err != nil {
		log.Fatal(err)
	}
	if err := e.Close(); err != nil {
		log.Fatal(err)
	}
}

func newAudioIntBuffer(r io.Reader) (*audio.IntBuffer, error) {
	buf := audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: 1,
			SampleRate:  8000,
		},
	}
	for {
		var sample int16
		err := binary.Read(r, binary.LittleEndian, &sample)
		switch {
		case err == io.EOF:
			return &buf, nil
		case err != nil:
			return nil, err
		}
		buf.Data = append(buf.Data, int(sample))
	}
}

// newAudioIntBuffer from another buffer 
func NewAudioIntBufferFromBuffer(buf []byte) (*audio.IntBuffer) {
	audioBuf := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: 1,
			SampleRate:  8000,
		},
		Data: make([]int, len(buf)/2),
	}
	for i := 0; i < len(buf); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(buf[i : i+2]))
		audioBuf.Data[i/2] = int(sample)
	}
	return audioBuf
}
