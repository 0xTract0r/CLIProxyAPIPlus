package executor

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: 400, msg: "streaming is unavailable for compact requests", requestScoped: true}
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	compat, err := e.expandCodexResponsesCompaction(auth, &req, &opts, model)
	if err != nil {
		return nil, statusErr{code: 400, msg: err.Error(), requestScoped: true}
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	if err = rejectSyntheticCompactionPreviousResponseID(ctx, req.Payload); err != nil {
		return nil, err
	}
	if e.shouldCompactCompatResponsesTrigger(compat, req, opts) {
		return e.executeCodexResponsesCompactionStream(ctx, auth, req, opts)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, model, auth)
	defer reporter.TrackFailure(ctx, &err)
	resp, body, err := e.sendOrdinaryCodex(ctx, auth, req, opts, "/responses", true, compat, reporter)
	if err != nil {
		return nil, err
	}
	scanCtx, cancelScanner := context.WithCancel(ctx)
	resp.Body = &ordinaryCodexOnceCloser{ReadCloser: resp.Body}
	scanned := startOrdinaryCodexScanner(scanCtx, resp.Body)
	var prefetched [][]byte
	if e.cfg != nil && e.cfg.Codex.StreamBootstrapBuffering {
		prefetched, err = bufferOrdinaryCodexBootstrap(ctx, scanned, e.cfg.Codex.StreamBootstrapTimeoutDuration(), e.modelLevelCooling(), func(raw []byte) { e.observeOrdinaryPolicyRefusal(ctx, auth, req.Model, raw) })
		if err != nil {
			cancelScanner()
			_ = resp.Body.Close()
			return nil, err
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		defer cancelScanner()
		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- chunk:
				return true
			}
		}
		fail := func(err error) { reporter.PublishFailure(ctx, err); send(cliproxyexecutor.StreamChunk{Err: err}) }
		next := func() (ordinaryCodexScannedLine, bool) {
			if len(prefetched) > 0 {
				v := prefetched[0]
				prefetched[0] = nil
				prefetched = prefetched[1:]
				return ordinaryCodexScannedLine{line: v}, true
			}
			select {
			case <-ctx.Done():
				return ordinaryCodexScannedLine{}, false
			case v, ok := <-scanned:
				return v, ok
			}
		}
		items := make(map[int64][]byte)
		var fallback [][]byte
		var param any
		sawOutput := false
		target := cliproxyexecutor.ResponseFormatOrSource(opts)
		for {
			item, ok := next()
			if !ok {
				break
			}
			if item.err != nil {
				fail(item.err)
				return
			}
			line := item.line
			data, isData := bytes.CutPrefix(line, []byte("data:"))
			done := false
			if isData {
				data = bytes.TrimSpace(data)
				if terminal, _, failed := codexTerminalFailureErrWithCooling(data, e.modelLevelCooling()); failed {
					e.observeOrdinaryPolicyRefusal(ctx, auth, req.Model, data)
					fail(terminal)
					return
				}
				reporter.ObserveResponseModel(data)
				event := helps.CodexResponseEventType(data)
				observeCodexTokenEvent(reporter, data)
				sawOutput = sawOutput || helps.HasMeaningfulCodexOutputDelta(data)
				if event == "response.output_item.done" {
					collectCodexOutputItemDone(data, items, &fallback)
				}
				done = event == "response.completed" || event == "response.done" || event == "response.incomplete"
				if done {
					if helps.IsCodexTerminalEmptyIncomplete(data, len(items)+len(fallback), sawOutput) {
						fail(newCodexEmptyIncompleteStreamError())
						return
					}
					data = patchCodexCompletedOutput(data, items, fallback)
					line = append([]byte("data: "), data...)
					if usage, ok := helps.ParseCodexUsageWithOutputItems(data, items, fallback); ok {
						reporter.Publish(ctx, usage)
					} else {
						reporter.EnsurePublished(ctx)
					}
					publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, data, items, fallback)
				}
			}
			for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FromString("codex"), target, req.Model, opts.OriginalRequest, body, line, &param) {
				if len(chunk) != 0 && !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
					return
				}
			}
			if done {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		fail(newCodexIncompleteStreamError())
	}()
	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: out}, nil
}

type ordinaryCodexScannedLine struct {
	line []byte
	err  error
}

func startOrdinaryCodexScanner(ctx context.Context, body io.ReadCloser) <-chan ordinaryCodexScannedLine {
	output := make(chan ordinaryCodexScannedLine)
	go func() {
		defer close(output)
		stop := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stop()
		scan := helps.NewCodexSSEScanner(ctx, body, 16<<20)
		for scan.Scan() {
			select {
			case <-ctx.Done():
				return
			case output <- ordinaryCodexScannedLine{line: bytes.Clone(scan.Bytes())}:
			}
		}
		if err := scan.Err(); err != nil {
			select {
			case <-ctx.Done():
			case output <- ordinaryCodexScannedLine{err: err}:
			}
		}
	}()
	return output
}

func bufferOrdinaryCodexBootstrap(ctx context.Context, in <-chan ordinaryCodexScannedLine, budget time.Duration, modelLevelCooling bool, observeFailure func([]byte)) ([][]byte, error) {
	var lines [][]byte
	var timer *time.Timer
	var deadline <-chan time.Time
	if budget > 0 {
		timer = time.NewTimer(budget)
		defer timer.Stop()
		deadline = timer.C
	}
	bytesRead := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return lines, nil
		case batch, ok := <-in:
			if !ok {
				return nil, newCodexIncompleteStreamError()
			}
			if batch.err != nil {
				return nil, batch.err
			}
			if data, ok := bytes.CutPrefix(batch.line, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				if terminal, _, failed := codexTerminalFailureErrWithCooling(data, modelLevelCooling); failed {
					if observeFailure != nil {
						observeFailure(data)
					}
					return nil, terminal
				}
				kind := helps.CodexResponseEventType(data)
				if kind == "response.completed" || kind == "response.done" || kind == "response.incomplete" ||
					(kind != "" && !isCodexBootstrapBufferableEvent(kind, data)) {
					return append(lines, batch.line), nil
				}
			}
			lines = append(lines, batch.line)
			bytesRead += len(batch.line)
			if len(lines) >= codexBootstrapMaxBufferedFrames || bytesRead >= codexBootstrapMaxBufferedBytes {
				return lines, nil
			}
		}
	}
}

type ordinaryCodexOnceCloser struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (c *ordinaryCodexOnceCloser) Close() error {
	c.once.Do(func() { c.err = c.ReadCloser.Close() })
	return c.err
}
