package main

import (
	"context"
	"errors"
	"io"

	"github.com/alessiosavi/go-dontwastetime/internal/mcpserver"
	"github.com/alessiosavi/go-dontwastetime/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveMCP owns only the protocol stream; application logs go to stderr.
func serveMCP(ctx context.Context, a *app, operator string, in io.ReadCloser, out io.Writer) error {
	svc, err := service.New(a.s, a.cfg, a.tx, service.Options{Operator: operator}, a.now)
	if err != nil {
		return err
	}
	err = mcpserver.New(svc, version).Run(ctx, &mcp.IOTransport{Reader: in, Writer: mcpWriter{out}})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// The caller owns stdout and may reuse it after the session closes.
type mcpWriter struct{ io.Writer }

func (mcpWriter) Close() error { return nil }
