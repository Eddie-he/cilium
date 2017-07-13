// Copyright 2016-2017 Authors of Cilium
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

package allocator

import (
	"fmt"
	"math/rand"
	"path"
	"testing"
	"time"

	"github.com/cilium/cilium/pkg/kvstore"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) {
	TestingT(t)
}

type AllocatorSuite struct{}

type AllocatorEtcdSuite struct {
	AllocatorSuite
}

var _ = Suite(&AllocatorEtcdSuite{})

func (e *AllocatorEtcdSuite) SetUpTest(c *C) {
	kvstore.SetupDummy("etcd")
}

type AllocatorConsulSuite struct {
	AllocatorSuite
}

var _ = Suite(&AllocatorConsulSuite{})

func (e *AllocatorConsulSuite) SetUpTest(c *C) {
	kvstore.SetupDummy("consul")
}

type TestType string

func (t TestType) GetKey() string { return string(t) }
func (t TestType) String() string { return string(t) }
func (t TestType) PutKey(v string) (AllocatorKey, error) {
	return TestType(v), nil
}

// Stolen from:
// https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
func init() {
	rand.Seed(time.Now().UnixNano())
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func (s *AllocatorSuite) TestSelectID(c *C) {
	allocatorName := randStringRunes(12)
	minID, maxID := ID(1), ID(5)
	a, err := NewAllocator(allocatorName, TestType(""), WithMin(minID), WithMax(maxID), WithSuffix("a"))
	c.Assert(err, IsNil)
	c.Assert(a, Not(IsNil))

	// allocate all available IDs
	for i := minID; i <= maxID; i++ {
		id, val := a.selectAvailableID()
		c.Assert(id, Equals, ID(i))
		c.Assert(val, Equals, i.String())
		a.cache[id] = TestType(fmt.Sprintf("key-%d", i))
	}

	// we should be out of IDs
	id, val := a.selectAvailableID()
	c.Assert(id, Equals, ID(0))
	c.Assert(val, Equals, "")
}

func (s *AllocatorSuite) BenchmarkAllocate(c *C) {
	allocatorName := randStringRunes(12)
	maxID := ID(c.N)
	allocator, err := NewAllocator(allocatorName, TestType(""), WithMax(maxID), WithSuffix("a"))
	c.Assert(err, IsNil)
	c.Assert(allocator, Not(IsNil))

	c.ResetTimer()
	for i := 0; i < c.N; i++ {
		_, _, err := allocator.Allocate(TestType(fmt.Sprintf("key%04d", i)))
		c.Assert(err, IsNil)
	}
	c.StopTimer()

	allocator.DeleteAllKeys()
}

func testAllocator(c *C, localCache bool) {
	allocatorName := randStringRunes(12)
	maxID := ID(256)
	allocator, err := NewAllocator(allocatorName, TestType(""), WithMax(maxID), WithSuffix("a"))
	allocator.skipCache = !localCache
	c.Assert(err, IsNil)
	c.Assert(allocator, Not(IsNil))

	// remove any keys which might be leftover
	allocator.DeleteAllKeys()

	// allocate all available IDs
	for i := ID(1); i <= maxID; i++ {
		key := TestType(fmt.Sprintf("key%04d", i))
		id, new, err := allocator.Allocate(key)
		c.Assert(err, IsNil)
		c.Assert(id, Not(Equals), 0)
		c.Assert(new, Equals, true)

		// refcnt must be 1
		c.Assert(allocator.localKeys.keys[key.GetKey()].refcnt, Equals, uint64(1))
	}

	// we should be out of id space here
	_, new, err := allocator.Allocate(TestType(fmt.Sprintf("key%04d", maxID+1)))
	c.Assert(err, Not(IsNil))
	c.Assert(new, Equals, false)

	// if using local cache, test reference counting
	if localCache {
		// allocate all IDs again using the same set of keys, refcnt should go to 2
		for i := ID(1); i <= maxID; i++ {
			key := TestType(fmt.Sprintf("key%04d", i))
			id, new, err := allocator.Allocate(key)
			c.Assert(err, IsNil)
			c.Assert(id, Not(Equals), 0)
			c.Assert(new, Equals, false)

			// refcnt must now be 2
			c.Assert(allocator.localKeys.keys[key.GetKey()].refcnt, Equals, uint64(2))
		}
	}

	// Create a 2nd allocator, refill it
	allocator2, err := NewAllocator(allocatorName, TestType(""), WithMax(maxID), WithSuffix("b"))
	c.Assert(err, IsNil)
	c.Assert(allocator2, Not(IsNil))

	// allocate all IDs again using the same set of keys, refcnt should go to 2
	for i := ID(1); i <= maxID; i++ {
		key := TestType(fmt.Sprintf("key%04d", i))
		id, new, err := allocator2.Allocate(key)
		c.Assert(err, IsNil)
		c.Assert(id, Not(Equals), 0)
		c.Assert(new, Equals, false)

		localKey := allocator2.localKeys.keys[key.GetKey()]
		c.Assert(localKey, Not(IsNil))

		// refcnt in the 2nd allocator is 1
		c.Assert(localKey.refcnt, Equals, uint64(1))

		allocator2.Release(key)
	}

	// release 2nd reference of all IDs
	for i := ID(1); i <= maxID; i++ {
		allocator.Release(TestType(fmt.Sprintf("key%04d", i)))
	}

	// refcnt should be back to 1
	for i := ID(1); i <= maxID; i++ {
		key := TestType(fmt.Sprintf("key%04d", i))
		c.Assert(allocator.localKeys.keys[key.GetKey()].refcnt, Equals, uint64(1))
	}

	// running the GC should not evict any entries
	allocator.runGC()

	v, err := kvstore.ListPrefix(allocator.idPrefix)
	c.Assert(err, IsNil)
	c.Assert(len(v), Equals, int(maxID))

	// release final reference of all IDs
	for i := ID(1); i <= maxID; i++ {
		allocator.Release(TestType(fmt.Sprintf("key%04d", i)))
	}

	// running the GC should evict all entries
	allocator.runGC()

	v, err = kvstore.ListPrefix(allocator.idPrefix)
	c.Assert(err, IsNil)
	c.Assert(len(v), Equals, 0)

	allocator.DeleteAllKeys()
	allocator.Delete()
	allocator2.Delete()
}

func (s *AllocatorSuite) TestAllocateCached(c *C) {
	testAllocator(c, true) // enable use of local cache
}

func (s *AllocatorSuite) TestAllocateNoCache(c *C) {
	//testAllocator(c, false) // disable use of local cache
}

func (s *AllocatorSuite) TestkeyToID(c *C) {
	allocatorName := randStringRunes(12)
	a, err := NewAllocator(allocatorName, TestType(""))
	c.Assert(err, IsNil)
	c.Assert(a, Not(IsNil))

	c.Assert(a.keyToID(path.Join(allocatorName, "invalid"), false), Equals, NoID)
	c.Assert(a.keyToID(path.Join(a.idPrefix, "invalid"), false), Equals, NoID)
	c.Assert(a.keyToID(path.Join(a.idPrefix, "10"), false), Equals, ID(10))
}
