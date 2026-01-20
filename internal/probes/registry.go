/*
Copyright 2025 Michael J. Saenz.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package probes

var defaultRegistry = NewRegistry()

// Registry is a registry of available probes.
type registry struct {
	executors map[string]ProbeExecutor
}

func NewRegistry() *registry {
	r := &registry{executors: make(map[string]ProbeExecutor)}
	r.executors["http"] = &HTTPProbeExecutor{}
	return r
}

func (r *registry) get(probeType string) ProbeExecutor {
	return r.executors[probeType]
}

func Get(probeType string) ProbeExecutor {
	return defaultRegistry.get(probeType)
}
