package gateway

type featureRegistry[T any] struct {
	finalToUpstream    map[string]string
	upstreamRegistered map[string]map[string]struct{}
	registered         map[string]T
	equal              func(a, b T) bool
}

func newFeatureRegistry[T any](equal func(a, b T) bool) featureRegistry[T] {
	return featureRegistry[T]{
		finalToUpstream:    make(map[string]string),
		upstreamRegistered: make(map[string]map[string]struct{}),
		registered:         make(map[string]T),
		equal:              equal,
	}
}

func (r *featureRegistry[T]) ensureUpstream(up string) {
	if r.upstreamRegistered[up] == nil {
		r.upstreamRegistered[up] = make(map[string]struct{})
	}
}

func (r *featureRegistry[T]) diff(upstream string, newPayloads map[string]T, rawNameByFinal map[string]string) (toRemove, toAddOrChange []string, collisions []Collision) {
	r.ensureUpstream(upstream)
	prev := r.upstreamRegistered[upstream]
	for name := range prev {
		if _, still := newPayloads[name]; !still {
			toRemove = append(toRemove, name)
		}
	}
	for name, newP := range newPayloads {
		owner, owned := r.finalToUpstream[name]
		if owned && owner != upstream {
			rawName := name
			if rawNameByFinal != nil {
				if n, ok := rawNameByFinal[name]; ok {
					rawName = n
				}
			}
			collisions = append(collisions, Collision{Upstream: owner, Tool: rawName, ResultName: name})
			continue
		}
		if owned && owner == upstream {
			if !r.equal(r.registered[name], newP) {
				toAddOrChange = append(toAddOrChange, name)
			}
		} else {
			toAddOrChange = append(toAddOrChange, name)
		}
	}
	return
}

func (r *featureRegistry[T]) apply(upstream string, toRemove []string, toAddOrChangePayloads map[string]T, finalNewSet map[string]struct{}) {
	r.ensureUpstream(upstream)
	for _, name := range toRemove {
		delete(r.finalToUpstream, name)
		delete(r.registered, name)
		delete(r.upstreamRegistered[upstream], name)
	}
	for name, p := range toAddOrChangePayloads {
		r.finalToUpstream[name] = upstream
		r.registered[name] = p
	}
	r.upstreamRegistered[upstream] = map[string]struct{}{}
	for n := range finalNewSet {
		if owner, ok := r.finalToUpstream[n]; ok && owner == upstream {
			r.upstreamRegistered[upstream][n] = struct{}{}
		}
	}
}
