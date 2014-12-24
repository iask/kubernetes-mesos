/*
Copyright 2014 Google Inc. All rights reserved.
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

// evolution of: https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.6/pkg/client/cache/fifo.go
package queue

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
)

type EventType int

const (
	ADD_EVENT EventType = 1 << iota
	UPDATE_EVENT
	DELETE_EVENT
	POP_EVENT
)

type Entry interface {
	Copyable
	Value() UniqueCopyable
	// types is a logically OR'd combination of EventType, e.g. ADD_EVENT|UPDATE_EVENT
	Is(types EventType) bool
}

type entry struct {
	value UniqueCopyable
	event EventType
}

type deletedEntry struct {
	*entry
	expiration time.Time
}

type Copyable interface {
	// return an independent copy (deep clone) of the current object
	Copy() Copyable
}

type UniqueID interface {
	GetUID() string
}

type UniqueCopyable interface {
	Copyable
	UniqueID
}

func (e *entry) Value() UniqueCopyable {
	return e.value
}

func (e *entry) Copy() Copyable {
	if e == nil {
		return nil
	}
	return &entry{e.value.Copy().(UniqueCopyable), e.event}
}

func (e *entry) Is(types EventType) bool {
	return types&e.event != 0
}

func (e *deletedEntry) Copy() Copyable {
	if e == nil {
		return nil
	}
	return &deletedEntry{e.entry.Copy().(*entry), e.expiration}
}

// deliver a message
type pigeon func(msg Entry)

func dead(msg Entry) {
	// intentionally blank
}

// HistoricalFIFO receives adds and updates from a Reflector, and puts them in a queue for
// FIFO order processing. If multiple adds/updates of a single item happen while
// an item is in the queue before it has been processed, it will only be
// processed once, and when it is processed, the most recent version will be
// processed. This can't be done with a channel.
type HistoricalFIFO struct {
	lock      sync.RWMutex
	cond      sync.Cond
	items     map[string]Entry // We depend on the property that items in the queue are in the set.
	queue     []string
	carrier   pigeon // may be dead, but never nil
	gcc       int
	lingerTTL time.Duration
}

// panics if obj doesn't implement UniqueCopyable; otherwise returns the same, typecast object
func checkType(obj interface{}) UniqueCopyable {
	if v, ok := obj.(UniqueCopyable); !ok {
		panic(fmt.Sprintf("Illegal object type, expected UniqueCopyable: %T", obj))
	} else {
		return v
	}
}

// Add inserts an item, and puts it in the queue. The item is only enqueued
// if it doesn't already exist in the set.
func (f *HistoricalFIFO) Add(id string, v interface{}) {
	obj := checkType(v)
	notifications := []Entry(nil)
	defer func() {
		for _, e := range notifications {
			f.carrier(e)
		}
	}()

	f.lock.Lock()
	defer f.lock.Unlock()

	if entry, exists := f.items[id]; !exists {
		f.queue = append(f.queue, id)
	} else {
		if entry.Is(DELETE_EVENT | POP_EVENT) {
			f.queue = append(f.queue, id)
		}
	}
	notifications = f.merge(id, obj)
	f.cond.Broadcast()
}

// Update is the same as Add in this implementation.
func (f *HistoricalFIFO) Update(id string, obj interface{}) {
	f.Add(id, obj)
}

// Add the item to the store, but only if there exists a prior entry for
// for the object in the store whose event type matches that given, and then
// only enqueued if it doesn't already exist in the set.
func (f *HistoricalFIFO) Readd(id string, v interface{}, t EventType) {
	obj := checkType(v)
	notifications := []Entry(nil)
	defer func() {
		for _, e := range notifications {
			f.carrier(e)
		}
	}()

	f.lock.Lock()
	defer f.lock.Unlock()

	if entry, exists := f.items[id]; exists {
		if !entry.Is(t) {
			return
		} else if entry.Is(DELETE_EVENT | POP_EVENT) {
			f.queue = append(f.queue, id)
		}
	}
	notifications = f.merge(id, obj)
	f.cond.Broadcast()
}

// Delete removes an item. It doesn't add it to the queue, because
// this implementation assumes the consumer only cares about the objects,
// not the order in which they were created/added.
func (f *HistoricalFIFO) Delete(id string) {
	deleteEvent := (Entry)(nil)
	defer func() {
		f.carrier(deleteEvent)
	}()

	f.lock.Lock()
	defer f.lock.Unlock()
	item, exists := f.items[id]
	if exists && !item.Is(DELETE_EVENT) {
		e := item.(*entry)
		e.event = DELETE_EVENT
		deleteEvent = &deletedEntry{e, time.Now().Add(f.lingerTTL)}
		f.items[id] = deleteEvent
	}
}

// List returns a list of all the items.
func (f *HistoricalFIFO) List() []interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()

	// TODO(jdef): slightly overallocates b/c of deleted items
	list := make([]interface{}, 0, len(f.queue))

	for _, entry := range f.items {
		if entry.Is(DELETE_EVENT | POP_EVENT) {
			continue
		}
		list = append(list, entry.Value().Copy())
	}
	return list
}

// ContainedIDs returns a util.StringSet containing all IDs of the stored items.
// This is a snapshot of a moment in time, and one should keep in mind that
// other go routines can add or remove items after you call this.
func (c *HistoricalFIFO) ContainedIDs() util.StringSet {
	c.lock.RLock()
	defer c.lock.RUnlock()
	set := util.StringSet{}
	for id, entry := range c.items {
		if entry.Is(DELETE_EVENT | POP_EVENT) {
			continue
		}
		set.Insert(id)
	}
	return set
}

// Get returns the requested item, or sets exists=false.
func (f *HistoricalFIFO) Get(id string) (interface{}, bool) {
	f.lock.RLock()
	defer f.lock.RUnlock()
	entry, exists := f.items[id]
	if exists && !entry.Is(DELETE_EVENT|POP_EVENT) {
		return entry.Value().Copy(), true
	}
	return nil, false
}

// Pop waits until an item is ready and returns it. If multiple items are
// ready, they are returned in the order in which they were added/updated.
// The item is removed from the queue (and the store) before it is returned,
// so if you don't succesfully process it, you need to add it back with Add().
func (f *HistoricalFIFO) Pop() interface{} {
	popEvent := (Entry)(nil)
	defer func() {
		f.carrier(popEvent)
	}()

	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			f.cond.Wait()
		}
		id := f.queue[0]
		f.queue = f.queue[1:]
		item, ok := f.items[id]
		if !ok || item.Is(DELETE_EVENT|POP_EVENT) {
			// Item may have been deleted subsequently.
			continue
		}
		value := item.Value()
		popEvent = &entry{value, POP_EVENT}
		f.items[id] = popEvent
		return value.Copy()
	}
}

// Replace will delete the contents of 'f', using instead the given map.
// 'f' takes ownersip of the map, you should not reference the map again
// after calling this function. f's queue is reset, too; upon return, it
// will contain the items in the map, in no particular order.
func (f *HistoricalFIFO) Replace(idToObj map[string]interface{}) {
	notifications := make([]Entry, 0, len(idToObj))
	defer func() {
		for _, e := range notifications {
			f.carrier(e)
		}
	}()

	f.lock.Lock()
	defer f.lock.Unlock()

	f.queue = f.queue[:0]
	now := time.Now()
	for id, v := range f.items {
		if _, exists := idToObj[id]; !exists && !v.Is(DELETE_EVENT) {
			// a non-deleted entry in the items list that doesn't show up in the
			// new list: mark it as deleted
			ent := v.(*entry)
			ent.event = DELETE_EVENT
			e := &deletedEntry{ent, now.Add(f.lingerTTL)}
			f.items[id] = e
			notifications = append(notifications, e)
		}
	}
	for id, v := range idToObj {
		obj := checkType(v)
		f.queue = append(f.queue, id)
		n := f.merge(id, obj)
		notifications = append(notifications, n...)
	}
	if len(f.queue) > 0 {
		f.cond.Broadcast()
	}
}

// garbage collect DELETEd items whose TTL has expired; the IDs of such items are removed
// from the queue. This impl assumes that caller has acquired state lock.
func (f *HistoricalFIFO) gc() {
	now := time.Now()
	deleted := make(map[string]struct{})
	for id, v := range f.items {
		if v.Is(DELETE_EVENT) {
			ent := v.(*deletedEntry)
			if ent.expiration.Before(now) {
				delete(f.items, id)
				deleted[id] = struct{}{}
			}
		}
	}
	// remove deleted items from the queue, will likely (slightly) overallocate here
	queue := make([]string, 0, len(f.queue))
	for _, id := range f.queue {
		if _, exists := deleted[id]; !exists {
			queue = append(queue, id)
		}
	}
	f.queue = queue
}

// Assumes that the caller has acquired the state lock.
func (f *HistoricalFIFO) merge(id string, obj UniqueCopyable) (notifications []Entry) {
	item, exists := f.items[id]
	now := time.Now()
	if !exists {
		e := &entry{obj.Copy().(UniqueCopyable), ADD_EVENT}
		f.items[id] = e
		notifications = append(notifications, e)
	} else {
		if !item.Is(DELETE_EVENT) && item.Value().GetUID() != obj.GetUID() {
			// hidden DELETE!
			// (1) append a DELETE
			// (2) append an ADD
			// .. and notify listeners in that order
			ent := item.(*entry)
			ent.event = DELETE_EVENT
			e1 := &deletedEntry{ent, now.Add(f.lingerTTL)}
			e2 := &entry{obj.Copy().(UniqueCopyable), ADD_EVENT}
			f.items[id] = e2
			notifications = append(notifications, e1, e2)
		} else if !reflect.DeepEqual(obj, item.Value()) {
			//TODO(jdef): it would be nice if we could rely on resource versions
			//instead of doing a DeepEqual. Maybe someday we'll be able to.
			e := &entry{obj.Copy().(UniqueCopyable), UPDATE_EVENT}
			f.items[id] = e
			notifications = append(notifications, e)
		}
	}
	// check for garbage collection
	f.gcc++
	if f.gcc%256 == 0 { //TODO(jdef): extract constant
		f.gcc = 0
		f.gc()
	}
	return
}

// NewFIFO returns a Store which can be used to queue up items to
// process. If a non-nil Mux is provided, then modifications to the
// the FIFO are delivered on a channel specific to this fifo.
func NewFIFO(ch chan<- Entry) *HistoricalFIFO {
	carrier := dead
	if ch != nil {
		carrier = func(msg Entry) {
			if msg != nil {
				ch <- msg.Copy().(Entry)
			}
		}
	}
	f := &HistoricalFIFO{
		items:     map[string]Entry{},
		queue:     []string{},
		carrier:   carrier,
		lingerTTL: 5 * time.Minute, // TODO(jdef): extract constant
	}
	f.cond.L = &f.lock
	return f
}
