//go:build integration

package api

import "time"

func (h *Interactions) AnswerWithin(d time.Duration) { h.answerWithin = d }
