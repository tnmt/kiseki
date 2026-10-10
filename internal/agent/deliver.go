package agent

import (
	"context"
	"errors"

	"github.com/tnmt/kiseki/internal/store"
)

const (
	batchMaxRecords = 500
	// batchMaxBytes keeps a request well under the server's 8 MiB body limit.
	batchMaxBytes = 4 << 20
)

func splitBatches(source string, records []store.Record) []Batch {
	var batches []Batch
	var cur []store.Record
	size := 0
	for _, r := range records {
		n := len(r.Payload) + len(r.RecordID) + 128
		if len(cur) > 0 && (len(cur) >= batchMaxRecords || size+n > batchMaxBytes) {
			batches = append(batches, Batch{Source: source, Records: cur})
			cur, size = nil, 0
		}
		cur = append(cur, r)
		size += n
	}
	if len(cur) > 0 {
		batches = append(batches, Batch{Source: source, Records: cur})
	}
	return batches
}

// deliver sends records in batches. A batch that cannot be delivered because
// the network or server is down goes to the spool and counts as handled. It
// returns an error when the server rejects a batch or the spool is unusable;
// the caller must then not advance its state.
func deliver(ctx context.Context, client *Client, spool *Spool, source string, records []store.Record) (sent, spooled int, err error) {
	for _, batch := range splitBatches(source, records) {
		if _, postErr := client.Post(ctx, batch); postErr != nil {
			var rejected *RejectedError
			if errors.As(postErr, &rejected) {
				return sent, spooled, postErr
			}
			if ctx.Err() != nil {
				return sent, spooled, ctx.Err()
			}
			if spoolErr := spool.Add(batch); spoolErr != nil {
				return sent, spooled, spoolErr
			}
			spooled += len(batch.Records)
			continue
		}
		sent += len(batch.Records)
	}
	return sent, spooled, nil
}
