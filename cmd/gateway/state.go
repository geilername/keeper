package main

import "github.com/geilername/keeper/internal/photostate"

// The fold from events to photo state lives in internal/photostate, shared
// with the indexer; the gateway stores its result in Postgres.
type (
	Photo = photostate.Photo
	Stage = photostate.Stage
)
