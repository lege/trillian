package cache

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/google/trillian"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/testonly"
)

var splitTestVector = []struct {
	inPath        []byte
	inPathLenBits int
	outPrefix     []byte
	outSuffixBits int
	outSuffix     byte
}{
	{[]byte{0x12, 0x34, 0x56, 0x7f}, 32, []byte{0x12, 0x34, 0x56}, 8, 0x7f},
	{[]byte{0x12, 0x34, 0x56, 0xff}, 29, []byte{0x12, 0x34, 0x56}, 5, 0xf8},
	{[]byte{0x12, 0x34, 0x56, 0xff}, 25, []byte{0x12, 0x34, 0x56}, 1, 0x80},
	{[]byte{0x12, 0x34, 0x56, 0x78}, 16, []byte{0x12}, 8, 0x34},
	{[]byte{0x12, 0x34, 0x56, 0x78}, 9, []byte{0x12}, 1, 0x00},
	{[]byte{0x12, 0x34, 0x56, 0x78}, 8, []byte{}, 8, 0x12},
	{[]byte{0x12, 0x34, 0x56, 0x78}, 7, []byte{}, 7, 0x12},
	{[]byte{0x12, 0x34, 0x56, 0x78}, 0, []byte{}, 0, 0},
	{[]byte{0x70}, 2, []byte{}, 2, 0x40},
	{[]byte{0x70}, 3, []byte{}, 3, 0x60},
	{[]byte{0x70}, 4, []byte{}, 4, 0x70},
	{[]byte{0x70}, 5, []byte{}, 5, 0x70},
	{[]byte{0x00, 0x03}, 16, []byte{0x00}, 8, 0x03},
	{[]byte{0x00, 0x03}, 15, []byte{0x00}, 7, 0x02},
}

func TestSplitNodeID(t *testing.T) {
	for i, v := range splitTestVector {
		n := storage.NewNodeIDFromHash(v.inPath)
		n.PrefixLenBits = v.inPathLenBits

		p, s := splitNodeID(n)
		if expected, got := v.outPrefix, p; !bytes.Equal(expected, got) {
			t.Fatalf("(test %d) Expected prefix %x, got %x", i, expected, got)
		}

		if expected, got := v.outSuffixBits, int(s.bits); expected != got {
			t.Fatalf("(test %d) Expected suffix num bits %d, got %d", i, expected, got)
		}

		if expected, got := v.outSuffix, s.path; expected != got {
			t.Fatalf("(test %d) Expected suffix path of %x, got %x", i, expected, got)
		}
	}
}

func TestCacheFillOnlyReadsSubtrees(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	m := NewMockNodeStorage(mockCtrl)
	c := NewSubtreeCache(PopulateMapSubtreeNodes(merkle.NewRFC6962TreeHasher(trillian.NewSHA256())))

	nodeID := storage.NewNodeIDFromHash([]byte("1234"))
	// When we loop around asking for all 0..32 bit prefix lengths of the above
	// NodeID, we should see just one "Get" request for each subtree.
	for b := 0; b < nodeID.PrefixLenBits; b += strataDepth {
		e := nodeID
		e.PrefixLenBits = b
		m.EXPECT().GetSubtree(testonly.NodeIDEq(e)).Return(&storage.SubtreeProto{
			Prefix: e.Path,
		}, nil)
	}

	for nodeID.PrefixLenBits > 0 {
		_, err := c.GetNodeHash(nodeID, m.GetSubtree)
		if err != nil {
			t.Fatalf("failed to get node hash: %v", err)
		}
		nodeID.PrefixLenBits--
	}
}

func noFetch(id storage.NodeID) (*storage.SubtreeProto, error) {
	return nil, errors.New("not supposed to read anything")
}

func TestCacheFlush(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	m := NewMockNodeStorage(mockCtrl)
	c := NewSubtreeCache(PopulateMapSubtreeNodes(merkle.NewRFC6962TreeHasher(trillian.NewSHA256())))

	nodeID := storage.NewNodeIDFromHash([]byte("1234"))
	expectedSetIDs := make(map[string]string)
	// When we loop around asking for all 0..32 bit prefix lengths of the above
	// NodeID, we should see just one "Get" request for each subtree.
	for b := 0; b < nodeID.PrefixLenBits; b += strataDepth {
		e := storage.NewNodeIDFromHash([]byte("1234"))
		//e := nodeID
		e.PrefixLenBits = b
		expectedSetIDs[e.String()] = "expected"
		m.EXPECT().GetSubtree(testonly.NodeIDEq(e)).Do(func(n storage.NodeID) {
			t.Logf("read %v", n)
		}).Return((*storage.SubtreeProto)(nil), nil)
	}
	m.EXPECT().SetSubtrees(gomock.Any()).Do(func(trees []*storage.SubtreeProto) {
		for _, s := range trees {
			subID := storage.NewNodeIDFromHash(s.Prefix)
			state, ok := expectedSetIDs[subID.String()]
			if !ok {
				t.Errorf("Unexpected write to subtree %s", subID.String())
			}
			switch state {
			case "expected":
				expectedSetIDs[subID.String()] = "met"
			case "met":
				t.Errorf("Second write to subtree %s", subID.String())
			default:
				t.Errorf("Unknown state for subtree %s: %s", subID.String(), state)
			}
			t.Logf("write %v -> (%d leaves)", subID, len(s.Leaves))
		}
	}).Return(nil)

	// Read nodes which touch the subtrees we'll write to:
	sibs := nodeID.Siblings()
	for s := range sibs {
		_, err := c.GetNodeHash(sibs[s], m.GetSubtree)
		if err != nil {
			t.Fatalf("failed to get node hash: %v", err)
		}
	}

	t.Logf("after sibs: %v", nodeID)

	// Write nodes
	for nodeID.PrefixLenBits > 0 {
		h := []byte(nodeID.String())
		err := c.SetNodeHash(nodeID, append([]byte("hash-"), h...), noFetch)
		if err != nil {
			t.Fatalf("failed to set node hash: %v", err)
		}
		nodeID.PrefixLenBits--
	}

	if err := c.Flush(m.SetSubtrees); err != nil {
		t.Fatalf("failed to flush cache: %v", err)
	}

	for k, v := range expectedSetIDs {
		switch v {
		case "expected":
			t.Errorf("Subtree %s remains unset", k)
		case "met":
			//
		default:
			t.Errorf("Unknown state for subtree %s: %s", k, v)
		}
	}
}

func TestSuffixSerializeFormat(t *testing.T) {
	s := Suffix{5, 0xae}
	if got, want := s.serialize(), "Ba4="; got != want {
		t.Fatalf("Got serialized suffix of %s, expected %s", got, want)
	}
}

func TestRepopulateMapSubtreeKAT(t *testing.T) {
	hasher := merkle.NewRFC6962TreeHasher(trillian.NewSHA256())
	populateTheThing := PopulateMapSubtreeNodes(hasher)
	pb, err := ioutil.ReadFile("../../testdata/map_good_subtree.pb")
	if err != nil {
		t.Fatalf("failed to read test data: %v", err)
	}
	goodSubtree := storage.SubtreeProto{}
	if err := proto.UnmarshalText(string(pb), &goodSubtree); err != nil {
		t.Fatalf("failed to unmarshal SubtreeProto: %v", err)
	}

	leavesOnly := storage.SubtreeProto{}
	if err := proto.UnmarshalText(string(pb), &leavesOnly); err != nil {
		t.Fatalf("failed to unmarshal SubtreeProto: %v", err)
	}
	// erase the internal nodes
	leavesOnly.InternalNodes = make(map[string][]byte)

	if err := populateTheThing(&leavesOnly); err != nil {
		t.Fatalf("failed to repopulate subtree: %v", err)
	}
	if got, want := trillian.Hash(leavesOnly.RootHash), trillian.Hash(goodSubtree.RootHash); !bytes.Equal(got, want) {
		t.Errorf("recalculated incorrect root: got %v, wanted %v", got, want)
	}
	if got, want := len(leavesOnly.InternalNodes), len(goodSubtree.InternalNodes); got != want {
		t.Errorf("recalculated tree has %d internal nodes, expected %d", got, want)
	}

	for k, v := range goodSubtree.InternalNodes {
		h, ok := leavesOnly.InternalNodes[k]
		if !ok {
			t.Errorf("Reconstructed tree missing internal node for %v", k)
			continue
		}
		if got, want := trillian.Hash(h), trillian.Hash(v); !bytes.Equal(got, want) {
			t.Errorf("Recalculated incorrect hash for node %v, got %v expected %v", k, got, want)
		}
		delete(leavesOnly.InternalNodes, k)
	}
	if numExtraNodes := len(leavesOnly.InternalNodes); numExtraNodes > 0 {
		t.Errorf("Reconstructed tree has %d unexpected extra nodes:", numExtraNodes)
		for k, _ := range leavesOnly.InternalNodes {
			rk, err := base64.StdEncoding.DecodeString(k)
			if err != nil {
				t.Errorf("  invalid base64: %v", err)
				continue
			}
			t.Errorf("  %v (%v)", k, rk)
		}
	}
}

func TestRepopulateLogSubtree(t *testing.T) {
	hasher := merkle.NewRFC6962TreeHasher(trillian.NewSHA256())
	populateTheThing := PopulateLogSubtreeNodes(hasher)
	cmt := merkle.NewCompactMerkleTree(hasher)
	cmtStorage := storage.SubtreeProto{
		Leaves:        make(map[string][]byte),
		InternalNodes: make(map[string][]byte),
	}
	s := storage.SubtreeProto{
		Leaves: make(map[string][]byte),
	}
	for numLeaves := int64(1); numLeaves < 255; numLeaves++ {
		// clear internal nodes
		s.InternalNodes = make(map[string][]byte)

		leaf := []byte(fmt.Sprintf("this is leaf %d", numLeaves))
		leafHash := hasher.Digest(leaf)
		cmt.AddLeafHash(leafHash, func(depth int, index int64, h trillian.Hash) {
			n, err := storage.NewNodeIDForTreeCoords(int64(depth), index, 8)
			if err != nil {
				t.Fatalf("failed to create nodeID for cmt tree: %v", err)
			}
			// Don't store leaves or the subtree root in InternalNodes
			if depth > 0 && depth < 8 {
				_, sfx := splitNodeID(n)
				cmtStorage.InternalNodes[sfx.serialize()] = h
			}
		})

		sfx, err := makeSuffixKey(8, numLeaves-1)
		if err != nil {
			t.Fatalf("failed to create suffix key: %v", err)
		}
		s.Leaves[sfx] = leafHash
		cmtStorage.Leaves[sfx] = leafHash

		if err := populateTheThing(&s); err != nil {
			t.Fatalf("failed populate subtree: %v", err)
		}

		if got, expected := trillian.Hash(s.RootHash), cmt.CurrentRoot(); !bytes.Equal(got, expected) {
			t.Fatalf("Got root %v for tree size %d, expected %v. subtree:\n%#v", got, numLeaves, expected, s.String())
		}

		if !reflect.DeepEqual(cmtStorage.InternalNodes, s.InternalNodes) {
			t.Fatalf("(it %d) CMT internal nodes are\n%v, but sparse internal nodes are\n%v", numLeaves, cmtStorage.InternalNodes, s.InternalNodes)
		}
	}
}

type logKATData struct {
	File      string
	NumLeaves int
}

func TestRepopulateLogSubtreeKAT(t *testing.T) {
	testVector := []logKATData{
		{"log_good_subtree_5.pb", 5},
		{"log_good_subtree_55.pb", 55},
	}

	for _, k := range testVector {
		runLogSubtreeKAT(t, k)
	}
}

func runLogSubtreeKAT(t *testing.T, data logKATData) {
	hasher := merkle.NewRFC6962TreeHasher(trillian.NewSHA256())
	populateTheThing := PopulateLogSubtreeNodes(hasher)
	pb, err := ioutil.ReadFile("../../testdata/" + data.File)
	if err != nil {
		t.Fatalf("failed to read test data: %v", err)
	}
	goodSubtree := storage.SubtreeProto{}
	if err := proto.UnmarshalText(string(pb), &goodSubtree); err != nil {
		t.Fatalf("failed to unmarshal SubtreeProto: %v", err)
	}
	t.Logf("good root %v", trillian.Hash(goodSubtree.RootHash))

	leavesOnly := storage.SubtreeProto{}
	if err := proto.UnmarshalText(string(pb), &leavesOnly); err != nil {
		t.Fatalf("failed to unmarshal SubtreeProto: %v", err)
	}
	// erase the internal nodes
	leavesOnly.InternalNodes = make(map[string][]byte)

	if err := populateTheThing(&leavesOnly); err != nil {
		t.Fatalf("failed to repopulate subtree: %v", err)
	}
	if got, want := trillian.Hash(leavesOnly.RootHash), trillian.Hash(goodSubtree.RootHash); !bytes.Equal(got, want) {
		t.Errorf("recalculated incorrect root: got %v, wanted %v", got, want)
	}
	if got, want := len(leavesOnly.InternalNodes), len(goodSubtree.InternalNodes); got != want {
		t.Errorf("recalculated tree has %d internal nodes, expected %d", got, want)
	}

	for k, v := range goodSubtree.InternalNodes {
		h, ok := leavesOnly.InternalNodes[k]
		if !ok {
			t.Errorf("Reconstructed tree missing internal node for %v", k)
			continue
		}
		if got, want := trillian.Hash(h), trillian.Hash(v); !bytes.Equal(got, want) {
			t.Errorf("Recalculated incorrect hash for node %v, got %v expected %v", k, got, want)
		}
		delete(leavesOnly.InternalNodes, k)
	}
	if numExtraNodes := len(leavesOnly.InternalNodes); numExtraNodes > 0 {
		t.Errorf("Reconstructed tree has %d unexpected extra nodes:", numExtraNodes)
		for k, _ := range leavesOnly.InternalNodes {
			rk, err := base64.StdEncoding.DecodeString(k)
			if err != nil {
				t.Errorf("  invalid base64: %v", err)
				continue
			}
			t.Errorf("  %v (%v)", k, rk)
		}
	}
}
