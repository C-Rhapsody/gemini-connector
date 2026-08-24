package main

import "sort"

// AdapterRegistry is the central lookup for platform adapters and their
// optional capabilities. Application services resolve transport features
// through it instead of depending on concrete adapter types.
type AdapterRegistry struct {
	adapters map[string]Messenger
}

func NewAdapterRegistry(adapters map[string]Messenger) *AdapterRegistry {
	copied := make(map[string]Messenger, len(adapters))
	for name, a := range adapters {
		copied[name] = a
	}
	return &AdapterRegistry{adapters: copied}
}

// Get returns the adapter registered for a platform.
func (r *AdapterRegistry) Get(platform string) (Messenger, bool) {
	a, ok := r.adapters[platform]
	return a, ok
}

// Interactive returns the platform's rich-interaction capability, or nil when
// the adapter does not support inline keyboards and callbacks.
func (r *AdapterRegistry) Interactive(platform string) InteractiveSender {
	if a, ok := r.adapters[platform]; ok {
		if is, ok := a.(InteractiveSender); ok {
			return is
		}
	}
	return nil
}

// Attachment returns the platform's file-upload capability, or nil.
func (r *AdapterRegistry) Attachment(platform string) AttachmentSender {
	if a, ok := r.adapters[platform]; ok {
		if as, ok := a.(AttachmentSender); ok {
			return as
		}
	}
	return nil
}

// Platforms lists the registered platforms in stable order.
func (r *AdapterRegistry) Platforms() []string {
	out := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
