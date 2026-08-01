package provider

import (
	"context"
	"iter"
	"sync"
)

// Replay serves scripted turns instead of calling a model. It is the test
// double for the agent loop: recorded sessions become fixtures ("the log is
// the mock"), and Requests captures what the loop actually asked for.
type Replay struct {
	Turns []ReplayTurn

	mtx      sync.Mutex
	Requests []Request
	next     int
}

type ReplayTurn struct {
	Blocks []Block
	Usage  Usage
	Stop   string
	Err    error
}

func (p *Replay) Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		p.mtx.Lock()
		p.Requests = append(p.Requests, req)
		if p.next >= len(p.Turns) {
			p.mtx.Unlock()
			yield(Chunk{}, &Error{Msg: "replay: out of scripted turns"})
			return
		}
		t := p.Turns[p.next]
		p.next++
		p.mtx.Unlock()
		if t.Err != nil {
			yield(Chunk{}, t.Err)
			return
		}
		for _, b := range t.Blocks {
			switch b.Type {
			case Text:
				if !yield(Chunk{Kind: KindText, Text: b.Text}, nil) {
					return
				}
			case Reasoning:
				if !yield(Chunk{Kind: KindReasoning, Text: b.Text}, nil) {
					return
				}
			}
			if !yield(Chunk{Kind: KindBlock, Block: &b}, nil) {
				return
			}
		}
		if !yield(Chunk{Kind: KindUsage, Usage: &t.Usage}, nil) {
			return
		}
		yield(Chunk{Kind: KindStop, Stop: t.Stop}, nil)
	}
}
