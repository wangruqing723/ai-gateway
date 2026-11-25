package main

import (
	"fmt"
	"strings"

	"ai-gateway/internal/metrics"
)

const (
	attemptDetailHeadLimit = 20
	attemptDetailTailLimit = 20
)

// appendAttemptDetail 在追加时只保留请求尝试明细的头部与尾部。
func appendAttemptDetail(reqLog *metrics.RequestLog, detail metrics.AttemptDetail) {
	reqLog.AttemptDetails = append(reqLog.AttemptDetails, detail)
	if len(reqLog.AttemptDetails) <= attemptDetailHeadLimit+attemptDetailTailLimit {
		return
	}

	copy(reqLog.AttemptDetails[attemptDetailHeadLimit:], reqLog.AttemptDetails[attemptDetailHeadLimit+1:])
	reqLog.AttemptDetails = reqLog.AttemptDetails[:attemptDetailHeadLimit+attemptDetailTailLimit]
	reqLog.AttemptDetailsOmitted++
}

// trailBuffer 在追加期间保留轨迹的头 20 段与最新 20 段。
type trailBuffer struct {
	head    [attemptDetailHeadLimit]string
	headLen int
	tail    [attemptDetailTailLimit]string
	tailLen int
	omitted int
	total   int
}

func (t *trailBuffer) Add(segment string) {
	t.total++
	if t.headLen < len(t.head) {
		t.head[t.headLen] = segment
		t.headLen++
		return
	}
	if t.tailLen < len(t.tail) {
		t.tail[t.tailLen] = segment
		t.tailLen++
		return
	}

	copy(t.tail[:], t.tail[1:])
	t.tail[len(t.tail)-1] = segment
	t.omitted++
}

func (t *trailBuffer) Len() int {
	return t.total
}

func (t *trailBuffer) String() string {
	segments := make([]string, 0, t.headLen+t.tailLen+1)
	segments = append(segments, t.head[:t.headLen]...)
	if t.omitted > 0 {
		segments = append(segments, fmt.Sprintf("…省略 %d 段…", t.omitted))
	}
	segments = append(segments, t.tail[:t.tailLen]...)
	return strings.Join(segments, " → ")
}
