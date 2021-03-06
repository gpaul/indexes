// Simple B+-Tree package. Meant for in memory indexes, and builds it
// in a log structure for values for optimal use of GC time. Also
// supports appending to an existing key's value.
package btree

import (
	"fmt"
	"io"

	"github.com/avisagie/indexes"
)

// B+ Tree. Consists of pages. Satisfies indexes.Index.
type Btree struct {
	pager  Pager
	values [][]byte
	root   int
	size   int64
}

type btreeIter struct {
	prefix   []byte
	pageIter PageIter
	page     Page
	b        *Btree
	done     bool
}

func (i *btreeIter) Next() (ok bool, key []byte, value []byte) {
	if i.done {
		return
	}

	ok, key, ref := i.pageIter.Next()
	if ok {
		return ok, key, i.b.values[ref]
	}

	// !ok can mean we're done iterating or that we're at the end
	// of this page. TODO ponder another return value that
	// signifies being done iterating explicitly.

	n := i.page.NextPage()
	if n == -1 {
		i.done = true
		return
	}

	i.page = i.b.pager.Get(n)
	i.pageIter = i.page.Start(i.prefix)
	ok, key, ref = i.pageIter.Next()
	if !ok {
		i.done = true
		return
	}
	return ok, key, i.b.values[ref]
}

func NewInMemoryBtree() indexes.Index {
	ret := &Btree{newInplacePager(), make([][]byte, 0), 0, 0}

	ref, root := ret.pager.New(false)
	ret.root = ref

	ref, _ = ret.pager.New(true)
	root.SetFirst(ref)

	return ret
}

func (b *Btree) search(key []byte) (ok bool, k Key, pageRefs []int) {
	pageRefs = make([]int, 0, 8)
	ref := b.root

	// keep track of the pageRefs we visit searching down the
	// tree.
	pageRefs = append(pageRefs, ref)

	for {
		p := b.pager.Get(ref)
		ok, k = p.Search(key)
		ref = k.Ref()

		// if it is a leaf, we're done
		if p.IsLeaf() {
			break
		}

		pageRefs = append(pageRefs, ref)
	}

	return
}

func (b *Btree) Get(key []byte) (ok bool, value []byte) {
	if key == nil || len(key) == 0 {
		panic("Illegal key nil")
	}

	ok, k, _ := b.search(key)
	if ok {
		value = b.values[k.Ref()]
	}

	return
}

func (b *Btree) Start(prefix []byte) (it indexes.Iter) {
	if prefix == nil {
		panic("Illegal key nil")
	}

	_, _, pageRefs := b.search(prefix)

	ref := pageRefs[len(pageRefs)-1]
	page := b.pager.Get(ref)

	return &btreeIter{prefix, page.Start(prefix), page, b, false}
}

func (b *Btree) split(key []byte, ref int, pageRefs []int) {
	pageRef := pageRefs[len(pageRefs)-1]
	page := b.pager.Get(pageRef)

	parentRef := pageRefs[len(pageRefs)-2]
	parent := b.pager.Get(parentRef)

	// Split the page
	newPageRef, newPage := b.pager.New(page.IsLeaf())
	splitKey := page.Split(newPageRef, newPage)

	newPage.SetNextPage(page.NextPage())
	page.SetNextPage(newPageRef)

	// Insert the key, decide in which of the resulting pages it
	// must go. Don't bother checking ok, after split there must
	// be space.
	if keyLess(key, splitKey) {
		page.Insert(key, ref)
	} else {
		newPage.Insert(key, ref)
	}

	ok := parent.Insert(splitKey, newPageRef)
	if !ok {
		if parentRef == b.root {
			if len(pageRefs) != 2 {
				panic("insane")
			}
			oldRootRef := b.root
			newRootRef, newRoot := b.pager.New(false)
			newRoot.SetFirst(oldRootRef)
			b.root = newRootRef
			b.split(splitKey, newPageRef, []int{newRootRef, parentRef})
		} else {
			b.split(splitKey, newPageRef, pageRefs[:len(pageRefs)-1])
		}
	}
}

func (b *Btree) Put(key []byte, valuev []byte) (replaced bool) {
	if key == nil || len(key) == 0 || valuev == nil {
		panic("Illegal nil key or value")
	}

	replaced, k, pageRefs := b.search(key)
	if replaced {
		// Overwrite the old value
		b.values[k.Ref()] = append(b.values[k.Ref()][:0], valuev...)
		return
	}

	// TODO factor out allocating space for values to the pager?
	value := copyBytes(valuev)

	vref := len(b.values)
	pageRef := pageRefs[len(pageRefs)-1]
	page := b.pager.Get(pageRef)
	ok := page.Insert(key, vref)
	if !ok {
		b.split(key, vref, pageRefs)
	}

	b.values = append(b.values, []byte{})
	b.values[vref] = append(b.values[vref], value...)

	b.size++

	return
}

func (b *Btree) Append(key []byte, value []byte) {
	if key == nil || len(key) == 0 || value == nil {
		panic("Illegal nil key or value")
	}

	ok, k, _ := b.search(key)
	if ok {
		b.values[k.Ref()] = append(b.values[k.Ref()], value...)
	} else {
		if b.Put(key, value) {
			panic("Did not expect to have to replace the value")
		}
	}
}

func (b *Btree) Size() int64 {
	return b.size
}

// recursively check sorting inside pages and that child pages
// only have keys that are greater than or equal to the keys
// that reference them.
func (b *Btree) checkPage(page Page, checkMinKey bool, minKey []byte, ref int, depth int) error {
	if page.IsLeaf() {
		prev := []byte{}
		for i := 0; i < page.Size(); i++ {
			k, r := page.GetKey(i)
			if !keyLess(prev, k) {
				return fmt.Errorf("Expect strict ordering, got violation %v >= %v", prev, k)
			}
			if r < 0 {
				return fmt.Errorf("value reference cannot be < 0")
			}
			prev = k
		}
	} else {
		prevk, prevr := page.GetKey(0)
		if prevr == -1 && page.Size() > 1 {
			return fmt.Errorf("Expected internal node to refer to other pages")
		}
		for i := 1; i < page.Size(); i++ {
			k, r := page.GetKey(i)
			if checkMinKey && !keyLess(minKey, k) {
				return fmt.Errorf("Expect parent key to be smaller or equal to all in referred to child page: got violation %v >= %v", prevk, minKey)
			}
			if !keyLess(prevk, k) {
				return fmt.Errorf("Expect strict ordering, got violation %v >= %v", prevk, k)
			}
			if r < 0 {
				return fmt.Errorf("value reference cannot be < 0")
			}
			if err := b.checkPage(b.pager.Get(r), true, k, r, depth+1); err != nil {
				return err
			}
		}
	}

	return nil
}

func (b *Btree) CheckConsistency() error {

	count := int64(0)

	iter := b.Start([]byte{})
	prev := []byte{}
	for {
		ok, k, _ := iter.Next()
		if !ok {
			break
		}
		if k == nil || len(k) == 0 {
			return fmt.Errorf("Got empty key %v", k)
		}
		if !keyLess(prev, k) {
			return fmt.Errorf("Expect strict ordering, got violation %v >= %v", prev, k)
		}
		count++
	}

	if count != b.Size() {
		return fmt.Errorf("Expected %d, got %d", b.Size(), count)
	}

	root := b.pager.Get(b.root)
	return b.checkPage(root, false, []byte{}, 0, 0)
}

func (b *Btree) appendPage(key []byte, ref int, pageRefs []int) {
	pageRef := pageRefs[len(pageRefs)-1]
	page := b.pager.Get(pageRef)

	parentRef := pageRefs[len(pageRefs)-2]
	parent := b.pager.Get(parentRef)

	newPageRef, newPage := b.pager.New(page.IsLeaf())
	page.SetNextPage(newPageRef)

	if page.IsLeaf() {
		newPage.Insert(key, ref)
	} else {
		newPage.SetFirst(ref)
	}

	ok := parent.Insert(key, newPageRef)
	if !ok {
		if parentRef == b.root {
			newRootRef, newRoot := b.pager.New(false)
			newRoot.SetFirst(b.root)
			oldRootRef := b.root
			b.root = newRootRef
			b.appendPage(key, newPageRef, []int{newRootRef, oldRootRef})
		} else {
			b.appendPage(key, newPageRef, pageRefs[:len(pageRefs)-1])
		}
	}
}

// Put a key that is strictly larger than the previous one. Assumes
// you're going to keep doing that and therefore does the bulk put
// operation.
func (b *Btree) PutNext(keyv, valuev []byte) {
	if keyv == nil || len(keyv) == 0 || valuev == nil {
		panic("Illegal nil key or value")
	}

	pageRefs := make([]int, 0, 8)
	pageRefs = append(pageRefs, b.root)
	page := b.pager.Get(b.root)
	for !page.IsLeaf() {
		k, r := page.GetKey(page.Size() - 1)
		if !keyLess(k, keyv) {
			panic(fmt.Sprint("out of order put:", keyv))
		}
		page = b.pager.Get(r)
		pageRefs = append(pageRefs, r)
	}

	vref := len(b.values)
	b.values = append(b.values, copyBytes(valuev))
	key := copyBytes(keyv)
	ok := page.Insert(key, vref)
	if !ok {
		b.appendPage(key, vref, pageRefs)
	}
	b.size++
}

func spaces(n int) string {
	ret := []byte("")
	for i := 0; i < n; i++ {
		ret = append(ret, '\t')
	}
	return string(ret)
}

func (b *Btree) dumpPage(out io.Writer, ref, depth int) {
	space := spaces(depth)
	page := b.pager.Get(ref)
	fmt.Fprintf(out, "%sPage %d, leaf:%v, %d keys:\n", space, ref, page.IsLeaf(), page.Size())
	for i := 0; i < page.Size(); i++ {
		k, r := page.GetKey(i)
		fmt.Fprintf(out, "%s\t%d: %v -> %d\n", space, i, k, r)
		if !page.IsLeaf() {
			b.dumpPage(out, r, depth+1)
		}
	}
}

func (b *Btree) Dump(out io.Writer) {
	b.dumpPage(out, b.root, 0)
}

type BtreeStats struct {
	Finds            int
	Comparisons      int
	FillRate         float64
	NumInternalPages int
	NumLeafPages     int
}

func (b *Btree) Stats() BtreeStats {
	return b.pager.(*inplacePager).Stats()
}
