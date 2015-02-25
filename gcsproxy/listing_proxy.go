// Copyright 2015 Google Inc. All Rights Reserved.
// Author: jacobsa@google.com (Aaron Jacobs)

package gcsproxy

import (
	"container/list"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jacobsa/gcloud/gcs"
	"github.com/jacobsa/gcloud/gcs/gcsutil"
	"github.com/jacobsa/gcsfuse/timeutil"
	"golang.org/x/net/context"
	"google.golang.org/cloud/storage"
)

// A view on a "directory" in GCS that caches listings and modifications.
//
// Directories are by convention defined by '/' characters in object names. A
// directory is uniquely identified by an object name prefix that ends with a
// '/', or the empty string for the root directory. Given such a prefix P, the
// contents of directory P are:
//
//  *  The "files" within the directory: all objects named N such that
//      *  P is a strict prefix of N.
//      *  The portion of N following the prefix P contains no slashes.
//
//  *  The immediate "sub-directories": all strings P' such that
//      *  P' is a legal directory prefix according to the definition above.
//      *  P is a strict prefix of P'.
//      *  The portion of P' following the prefix P contains exactly one slash.
//      *  There is at least one objcet with name N such that N has P' as a
//         prefix.
//
// So for example, imagine a bucket contains the following objects:
//
//  *  burrito/
//  *  enchilada/
//  *  enchilada/0
//  *  enchilada/1
//  *  queso/carne/carnitas
//  *  queso/carne/nachos/
//  *  taco
//
// Then the directory structure looks like the following, where a trailing
// slash indicates a directory and the top level is the contents of the root
// directory:
//
//     burrito/
//     enchilada/
//         0
//         1
//     queso/
//         carne/
//             carnitas
//             nachos/
//     taco
//
// In particular, note that some directories are explicitly defined by a
// placeholder object, whether empty (burrito/, queso/carne/nachos/) or
// non-empty (enchilada/), and others are implicitly defined by
// their children (queso/carne/).
//
// Not safe for concurrent access. The user must provide external
// synchronization if necessary.
type ListingProxy struct {
	/////////////////////////
	// Dependencies
	/////////////////////////

	bucket gcs.Bucket
	clock  timeutil.Clock

	/////////////////////////
	// Constant data
	/////////////////////////

	// INVARIANT: isDirName(name)
	name string

	/////////////////////////
	// Mutable state
	/////////////////////////

	// Our current best understanding of the contents of the directory in GCS,
	// formed by listing the bucket and then patching according to child
	// modification records at the time, and patched since then by subsequent
	// modifications.
	//
	// The time after which this should be generated anew from a new listing is
	// also stored. This is set to the time at which the listing completed plus
	// the listing cache TTL.
	//
	// Sub-directories are of type string, and objects are of type
	// *storage.Object.
	//
	// INVARIANT: contents != nil
	// INVARIANT: All values are of type string or *storage.Object.
	// INVARIANT: For all string values v, checkSubdirName(v) == nil
	// INVARIANT: For all object values o, checkObjectName(o.Name) != nil
	// INVARIANT: All entries are indexed by the correct name.
	contents           map[string]interface{}
	contentsExpiration time.Time

	// A collection of children that have recently been added or removed locally
	// and the time at which it happened, ordered by the sequence in which it
	// happened. Elements M with M.node == nil are removals; all others are
	// additions.
	//
	// For a record M in this list with M's age less than the modification TTL,
	// any listing from the bucket should be augmented by pretending M just
	// happened.
	//
	// INVARIANT: All elements are of type childModification.
	// INVARIANT: Contains no duplicate names.
	// INVARIANT: For each M with M.node == nil, contents does not contain M.name.
	// INVARIANT: For each M with M.node != nil, contents[M.name] == M.node.
	childModifications list.List

	// An index of childModifications by name.
	//
	// INVARIANT: childModificationsIndex != nil
	// INVARIANT: For all names N in the map, the indexed modification has name N.
	// INVARIANT: Contains exactly the set of names in childModifications.
	childModificationsIndex map[string]*list.Element
}

// See ListingProxy.childModifications.
type childModification struct {
	expiration time.Time
	name       string

	// INVARIANT: node == nil or node is of type string or *storage.Object
	node interface{}
}

// How long we cache the most recent listing for a particular directory from
// GCS before regarding it as stale.
//
// Intended to paper over performance issues caused by quick follow-up calls;
// for example when the fuse VFS performs a readdir followed quickly by a
// lookup for each child. The drawback is that this increases the time before a
// write by a foreign machine within a recently-listed directory will be seen
// locally.
//
// TODO(jacobsa): Do we need this at all? Maybe the VFS layer does appropriate
// caching. Experiment with setting it to zero or ripping out the code.
//
// TODO(jacobsa): Set this according to real-world performance issues when the
// kernel does e.g. ReadDir followed by Lookup. Can probably be set quite
// small.
//
// TODO(jacobsa): Can this be moved to a decorator implementation of gcs.Bucket
// instead of living here?
const ListingProxy_ListingCacheTTL = 10 * time.Second

// How long we remember that we took some action on the contents of a directory
// (linking or unlinking), and pretend the action is reflected in the listing
// even if it is not reflected in a call to Bucket.ListObjects.
//
// Intended to paper over the fact that GCS doesn't offer list-your-own-writes
// consistency: it may be an arbitrarily long time before you see the creation
// or deletion of an object in a subsequent listing, and even if you see it in
// one listing you may not see it in the next. The drawback is that foreign
// modifications to recently-locally-modified directories will not be reflected
// locally for awhile.
//
// TODO(jacobsa): Set this according to information about listing staleness
// distributions from the GCS team.
//
// TODO(jacobsa): Can this be moved to a decorator implementation of gcs.Bucket
// instead of living here?
const ListingProxy_ModificationMemoryTTL = 5 * time.Minute

// Create a listing proxy object for the directory identified by the given
// prefix (see comments on ListingProxy). The supplied clock will be used for
// cache TTLs.
func NewListingProxy(
	bucket gcs.Bucket,
	clock timeutil.Clock,
	dir string) (lp *ListingProxy, err error) {
	// Make sure the directory name is legal.
	if !isDirName(dir) {
		err = fmt.Errorf("Illegal directory name: %s", dir)
		return
	}

	// Create the object.
	lp = &ListingProxy{
		bucket:                  bucket,
		clock:                   clock,
		name:                    dir,
		contents:                make(map[string]interface{}),
		childModificationsIndex: make(map[string]*list.Element),
	}

	return
}

// Return the directory prefix with which this object was configured.
func (lp *ListingProxy) Name() string {
	return lp.name
}

// Panic if any internal invariants are violated. Careful users can call this
// at appropriate times to help debug weirdness. Consider using
// syncutil.InvariantMutex to automate the process.
func (lp *ListingProxy) CheckInvariants() {
	// Check the name.
	if !isDirName(lp.name) {
		panic("Illegal name")
	}

	// Check that maps are non-nil.
	if lp.contents == nil || lp.childModificationsIndex == nil {
		panic("Expected contents and childModificationsIndex to be non-nil.")
	}

	// Check each element of the contents map.
	for k, node := range lp.contents {
		// Check that the key is legal.
		if !(strings.HasPrefix(k, lp.name) && k != lp.name) {
			panic(fmt.Sprintf("Name %s is not a strict prefix of key %s", lp.name, k))
		}

		// Type-specific logic
		switch typedNode := node.(type) {
		default:
			panic(fmt.Sprintf("Bad type for node: %v", node))

		case string:
			// Sub-directory
			if k != typedNode {
				panic(fmt.Sprintf("Name mismatch: %s vs. %s", k, typedNode))
			}

			if err := lp.checkSubdirName(typedNode); err != nil {
				panic("Illegal directory name: " + typedNode)
			}

		case *storage.Object:
			if k != typedNode.Name {
				panic(fmt.Sprintf("Name mismatch: %s vs. %s", k, typedNode.Name))
			}

			if err := lp.checkObjectName(typedNode.Name); err != nil {
				panic("Illegal object name: " + typedNode.Name)
			}
		}
	}

	// Check each child modification. Build a list of names we've seen while
	// doing so.
	var listNames sort.StringSlice
	for e := lp.childModifications.Front(); e != nil; e = e.Next() {
		m := e.Value.(childModification)
		listNames = append(listNames, m.name)

		if m.node == nil {
			if n, ok := lp.contents[m.name]; ok {
				panic(fmt.Sprintf("lp.contents[%s] == %v for removal", m.name, n))
			}
		} else {
			if n := lp.contents[m.name]; n != m.node {
				panic(fmt.Sprintf("lp.contents[%s] == %v, not %v", m.name, n, m.node))
			}
		}
	}

	sort.Sort(listNames)

	// Check that there were no duplicate names.
	for i, name := range listNames {
		if i == 0 {
			continue
		}

		if name == listNames[i-1] {
			panic("Duplicated name in childModifications: " + name)
		}
	}

	// Check the index. Build a list of names it contains While doing so.
	var indexNames sort.StringSlice
	for name, e := range lp.childModificationsIndex {
		indexNames = append(indexNames, name)

		m := e.Value.(childModification)
		if m.name != name {
			panic(fmt.Sprintf("Index name mismatch: %s vs. %s", m.name, name))
		}
	}

	sort.Sort(indexNames)

	// Check that the index contains the same set of names.
	if !reflect.DeepEqual(listNames, indexNames) {
		panic(fmt.Sprintf("Names mismatch:\n%v\n%v", listNames, indexNames))
	}
}

// Obtain a listing of the objects directly within the directory and the
// immediate sub-directories. (See comments on ListingProxy for precise
// semantics.) Object and sub-directory names are fully specified, not
// relative.
//
// This listing reflects any additions and removals set up with NoteNewObject,
// NoteNewSubdirectory, or NoteRemoval.
func (lp *ListingProxy) List(
	ctx context.Context) (objects []*storage.Object, subdirs []string, err error) {
	// Make sure lp.contents is valid.
	if err = lp.ensureContents(ctx); err != nil {
		return
	}

	// Read out the contents.
	for name, node := range lp.contents {
		switch typedNode := node.(type) {
		case *storage.Object:
			objects = append(objects, typedNode)

		case string:
			subdirs = append(subdirs, name)
		}
	}

	return
}

// Note that an object has been added to the directory, overriding any previous
// additions or removals with the same name. For awhile after this call, the
// response to a call to List will contain this object even if it is not
// present in a listing from the underlying bucket.
func (lp *ListingProxy) NoteNewObject(o *storage.Object) (err error) {
	name := o.Name

	// When we're finished, trim any expired modifications.
	defer lp.cleanChildModifications()

	// Make sure the object has a legal name.
	if err = lp.checkObjectName(name); err != nil {
		err = fmt.Errorf("Illegal object name (%v): %s", err, name)
		return
	}

	// Delete any existing record for this name.
	if e, ok := lp.childModificationsIndex[name]; ok {
		lp.childModifications.Remove(e)
		delete(lp.childModificationsIndex, name)
	}

	// Add a record.
	m := childModification{
		expiration: lp.clock.Now().Add(ListingProxy_ModificationMemoryTTL),
		name:       name,
		node:       o,
	}

	lp.childModificationsIndex[m.name] = lp.childModifications.PushBack(m)

	// Ensure the record is reflected in the contents.
	lp.playBackModification(m)

	return
}

// Note that a sub-directory has been added to the directory, overriding any
// previous additions or removals with the same name. For awhile after this
// call, the response to a call to List will contain this object even if it is
// not present in a listing from the underlying bucket.
//
// The name must be a legal directory prefix for a sub-directory of this
// directory. See notes on ListingProxy for more details.
func (lp *ListingProxy) NoteNewSubdirectory(name string) (err error) {
	// When we're finished, trim any expired modifications.
	defer lp.cleanChildModifications()

	// Make sure the object has a legal name.
	if err = lp.checkSubdirName(name); err != nil {
		err = fmt.Errorf("Illegal sub-directory name (%v): %s", err, name)
		return
	}

	err = errors.New("TODO: Implement NoteNewSubdirectory.")
	return
}

// Note that an object or directory prefix has been removed from the directory,
// overriding any previous additions or removals. For awhile after this call,
// the response to a call to List will not contain this name even if it is
// present in a listing from the underlying bucket.
func (lp *ListingProxy) NoteRemoval(name string) (err error) {
	// When we're finished, trim any expired modifications.
	defer lp.cleanChildModifications()

	err = errors.New("TODO: Implement NoteRemoval.")
	return
}

func isDirName(name string) bool {
	return name == "" || name[len(name)-1] == '/'
}

func (lp *ListingProxy) checkObjectName(name string) (err error) {
	if isDirName(name) {
		err = errors.New("Not an object name.")
		return
	}

	trimmed := strings.TrimPrefix(name, lp.name)
	if name == trimmed {
		err = errors.New("Not a descendant.")
		return
	}

	if strings.IndexByte(trimmed, '/') >= 0 {
		err = errors.New("Not a direct descendant.")
		return
	}

	return
}

func (lp *ListingProxy) checkSubdirName(name string) (err error) {
	if !isDirName(name) {
		err = errors.New("Not a directory name.")
		return
	}

	trimmed := strings.TrimPrefix(name, lp.name)
	if name == lp.name || name == trimmed {
		err = errors.New("Not a descendant.")
		return
	}

	if strings.IndexByte(trimmed, '/') != len(trimmed)-1 {
		err = errors.New("Not a direct descendant.")
		return
	}

	return
}

// If lp.contents is up to date, do nothing. Otherwise, regenerate it.
func (lp *ListingProxy) ensureContents(ctx context.Context) (err error) {
	// Is the map up to date?
	if lp.clock.Now().Before(lp.contentsExpiration) {
		return
	}

	// We will build a new map.
	contents := make(map[string]interface{})

	// List the directory.
	query := &storage.Query{
		Delimiter: "/",
		Prefix:    lp.name,
	}

	objects, subdirs, err := gcsutil.List(ctx, lp.bucket, query)
	if err != nil {
		err = fmt.Errorf("gcsutil.List: %v", err)
		return
	}

	// Process the returned objects.
	for _, o := range objects {
		// Special case: a placeholder object for the directory itself will show up
		// in the result, but we don't want it in our listing.
		if o.Name == lp.name {
			continue
		}

		// Make sure the object name is legal.
		if err = lp.checkObjectName(o.Name); err != nil {
			err = fmt.Errorf("List returned bad object name (%v): %s", err, o.Name)
			return
		}

		contents[o.Name] = o
	}

	// Process the returned prefixes.
	for _, subdir := range subdirs {
		// Directory names must be legal.
		if err = lp.checkSubdirName(subdir); err != nil {
			err = fmt.Errorf(
				"List returned bad sub-dir name (%v): %s",
				err,
				subdir)
			return
		}

		// Make sure the directory is a strict descendant.
		if !(strings.HasPrefix(subdir, lp.name) && subdir != lp.name) {
			err = fmt.Errorf("List returned non-descendant directory: %s", subdir)
			return
		}

		contents[subdir] = subdir
	}

	// Trim any expired modifications.
	lp.cleanChildModifications()

	// Swap in the new map and update the expiration time.
	lp.contents = contents
	lp.contentsExpiration = lp.clock.Now().Add(ListingProxy_ListingCacheTTL)

	// Play back child modifications.
	for e := lp.childModifications.Front(); e != nil; e = e.Next() {
		lp.playBackModification(e.Value.(childModification))
	}

	return
}

func (lp *ListingProxy) cleanChildModifications() {
	now := lp.clock.Now()

	// The simple way: build a list of names of expired modifications to remove.
	var names []string
	for e := lp.childModifications.Front(); e != nil; e = e.Next() {
		m := e.Value.(childModification)

		// Stop when we hit the first non-expired element. There may be expired
		// ones further on if time is not monotonic, but meh.
		if now.Before(m.expiration) {
			break
		}

		names = append(names, m.name)
	}

	// Remove each name we noted above.
	for _, name := range names {
		e := lp.childModificationsIndex[name]
		lp.childModifications.Remove(e)
		delete(lp.childModificationsIndex, name)
	}
}

func (lp *ListingProxy) playBackModification(m childModification) {
	// Removal?
	if m.node == nil {
		delete(lp.contents, m.name)
		return
	}

	lp.contents[m.name] = m.node
}
