// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package web embeds the static frontend assets served at the root of the visualizer.
package web

import (
	"embed"
	"io/fs"
)

//go:embed public
var publicFS embed.FS

// PublicFS returns an fs.FS rooted at web/public.
func PublicFS() fs.FS {
	sub, err := fs.Sub(publicFS, "public")
	if err != nil {
		panic(err)
	}
	return sub
}
