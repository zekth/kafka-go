package kafka

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/segmentio/kafka-go/protocol/deletetopics"
)

// DeleteTopicsRequest represents a request sent to a kafka broker to delete
// topics.
type DeleteTopicsRequest struct {
	// Address of the kafka broker to send the request to.
	Addr net.Addr

	// Names of topics to delete.
	Topics []string
}

// DeleteTopicsResponse represents a response from a kafka broker to a topic
// deletion request.
type DeleteTopicsResponse struct {
	// The amount of time that the broker throttled the request.
	//
	// This field will be zero if the kafka broker did not support the
	// DeleteTopics API in version 1 or above.
	Throttle time.Duration

	// Mapping of topic names to errors that occurred while attempting to delete
	// the topics.
	//
	// The errors contain the kafka error code. Programs may use the standard
	// errors.Is function to test the error against kafka error codes.
	Errors map[string]error
}

// DeleteTopics sends a topic deletion request to a kafka broker and returns the
// response.
func (c *Client) DeleteTopics(ctx context.Context, req *DeleteTopicsRequest) (*DeleteTopicsResponse, error) {
	m, err := c.roundTrip(ctx, req.Addr, &deletetopics.Request{
		TopicNames: req.Topics,
		TimeoutMs:  c.timeoutMs(ctx, defaultDeleteTopicsTimeout),
	})

	if err != nil {
		return nil, fmt.Errorf("kafka.(*Client).DeleteTopics: %w", err)
	}

	res := m.(*deletetopics.Response)
	ret := &DeleteTopicsResponse{
		Throttle: makeDuration(res.ThrottleTimeMs),
		Errors:   make(map[string]error, len(res.Responses)),
	}

	for _, t := range res.Responses {
		if t.ErrorCode == 0 {
			ret.Errors[t.Name] = nil
		} else {
			ret.Errors[t.Name] = Error(t.ErrorCode)
		}
	}

	return ret, nil
}

// See http://kafka.apache.org/protocol.html#The_Messages_DeleteTopics
type deleteTopicsRequest struct {
	// Topics holds the topic names
	Topics []string

	// Timeout holds the time in ms to wait for a topic to be completely deleted
	// on the controller node. Values <= 0 will trigger topic deletion and return
	// immediately.
	Timeout int32
}

func (t deleteTopicsRequest) size() int32 {
	return sizeofStringArray(t.Topics) +
		sizeofInt32(t.Timeout)
}

func (t deleteTopicsRequest) writeTo(wb *writeBuffer) {
	wb.writeStringArray(t.Topics)
	wb.writeInt32(t.Timeout)
}

type deleteTopicsResponse struct {
	v apiVersion // v0, v1

	ThrottleTime int32
	// TopicErrorCodes holds per topic error codes
	TopicErrorCodes []deleteTopicsResponseV0TopicErrorCode
}

func (t deleteTopicsResponse) size() int32 {
	sz := sizeofArray(len(t.TopicErrorCodes), func(i int) int32 { return t.TopicErrorCodes[i].size() })
	if t.v >= v1 {
		sz += sizeofInt32(t.ThrottleTime)
	}
	return sz
}

func (t *deleteTopicsResponse) readFrom(r *bufio.Reader, size int) (remain int, err error) {
	fn := func(withReader *bufio.Reader, withSize int) (fnRemain int, fnErr error) {
		var item deleteTopicsResponseV0TopicErrorCode
		if fnRemain, fnErr = (&item).readFrom(withReader, withSize); fnErr != nil {
			return
		}
		t.TopicErrorCodes = append(t.TopicErrorCodes, item)
		return
	}
	remain = size
	if t.v >= v1 {
		if remain, err = readInt32(r, size, &t.ThrottleTime); err != nil {
			return
		}
	}
	if remain, err = readArrayWith(r, remain, fn); err != nil {
		return
	}
	return
}

func (t deleteTopicsResponse) writeTo(wb *writeBuffer) {
	if t.v >= v1 {
		wb.writeInt32(t.ThrottleTime)
	}
	wb.writeArray(len(t.TopicErrorCodes), func(i int) { t.TopicErrorCodes[i].writeTo(wb) })
}

type deleteTopicsResponseV0TopicErrorCode struct {
	// Topic holds the topic name
	Topic string

	// ErrorCode holds the error code
	ErrorCode int16
}

func (t deleteTopicsResponseV0TopicErrorCode) size() int32 {
	return sizeofString(t.Topic) +
		sizeofInt16(t.ErrorCode)
}

func (t *deleteTopicsResponseV0TopicErrorCode) readFrom(r *bufio.Reader, size int) (remain int, err error) {
	if remain, err = readString(r, size, &t.Topic); err != nil {
		return
	}
	if remain, err = readInt16(r, remain, &t.ErrorCode); err != nil {
		return
	}
	return
}

func (t deleteTopicsResponseV0TopicErrorCode) writeTo(wb *writeBuffer) {
	wb.writeString(t.Topic)
	wb.writeInt16(t.ErrorCode)
}

// deleteTopics deletes the specified topics.
//
// See http://kafka.apache.org/protocol.html#The_Messages_DeleteTopics
func (c *Conn) deleteTopics(request deleteTopicsRequest) (deleteTopicsResponse, error) {
	version, err := c.negotiateVersion(deleteTopics, v0, v1)
	if err != nil {
		return deleteTopicsResponse{}, err
	}

	response := deleteTopicsResponse{
		v: version,
	}

	err = c.writeOperation(
		func(deadline time.Time, id int32) error {
			if request.Timeout == 0 {
				now := time.Now()
				deadline = adjustDeadlineForRTT(deadline, now, defaultRTT)
				request.Timeout = milliseconds(deadlineToTimeout(deadline, now))
			}
			return c.writeRequest(deleteTopics, version, id, request)
		},
		func(deadline time.Time, size int) error {
			return expectZeroSize(func() (remain int, err error) {
				return (&response).readFrom(&c.rbuf, size)
			}())
		},
	)
	if err != nil {
		return deleteTopicsResponse{}, err
	}
	for _, c := range response.TopicErrorCodes {
		if c.ErrorCode != 0 {
			return response, Error(c.ErrorCode)
		}
	}
	return response, nil
}
