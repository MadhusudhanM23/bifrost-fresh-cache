// Package backend builds the vector stores under test through Bifrost's own
// constructor (vectorstore.NewVectorStore) and adds a read-only side channel
// to each server for the things the VectorStore interface cannot report:
// versions, index layout, query plans, and whether background indexing has
// finished. Nothing in the side channel is ever timed.
package backend

import (
	"context"
	"fmt"
	"strconv"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
)

// Options holds connection settings for every supported backend.
type Options struct {
	PgHost     string
	PgPort     int
	PgUser     string
	PgPassword string
	PgDatabase string
	PgSSLMode  string
	PgMaxConns int

	QdrantHost string
	QdrantPort int

	RedisAddr string
}

// SampleQuery is a GetNearest call to explain (pgvector only).
type SampleQuery struct {
	Vector    []float32
	Filters   []vectorstore.Query
	Threshold float64
	Limit     int64
}

// Backend is one store under test plus its diagnostics.
type Backend struct {
	Name  string
	Store vectorstore.VectorStore
	// Server reports server versions and settings.
	Server func(ctx context.Context) map[string]any
	// Ready brings a freshly loaded namespace to a steady state before it is
	// queried and says what it did.
	Ready func(ctx context.Context, namespace string) (string, error)
	// Describe reports the namespace's size and indexes.
	Describe func(ctx context.Context, namespace string) map[string]any
	// Explain returns query plans for sample GetNearest calls; nil when the
	// backend has no notion of a plan.
	Explain func(ctx context.Context, namespace string, samples map[string]SampleQuery) map[string]string
	// Close releases the store and the side channel.
	Close func()
}

// New builds the named backend.
func New(ctx context.Context, name string, opts Options, logger schemas.Logger) (*Backend, error) {
	switch name {
	case "pgvector":
		return newPgVector(ctx, opts, logger)
	case "qdrant":
		return newQdrant(ctx, opts, logger)
	case "redis":
		return newRedis(ctx, opts, logger)
	}
	return nil, fmt.Errorf("unknown backend %q (want pgvector, qdrant or redis)", name)
}

func secret(v string) schemas.SecretVar { return *schemas.NewSecretVar(v) }

func itoa(v int) string { return strconv.Itoa(v) }

func newStore(ctx context.Context, typ vectorstore.VectorStoreType, config any, logger schemas.Logger) (vectorstore.VectorStore, error) {
	return vectorstore.NewVectorStore(ctx, &vectorstore.Config{Enabled: true, Type: typ, Config: config}, logger)
}
