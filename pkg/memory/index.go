package memory

import (
	"math"
	"sort"
	"strings"
)

// A local BM25 inverted index — the reason memory search beats grep:
// grep needs the exact byte sequence in order; the index matches any
// query term (CJK expands to bigrams, so "上下文" hits a document that
// only says "上下文窗口" even when the query also carries other words),
// ranks by term specificity and field weight, and runs in microseconds
// over an in-memory map instead of walking files per query.
//
// Field weights: name 3.0, tags 2.5, description 2.0, body 1.0 — a hit
// in the title outranks the same word buried in prose.

const (
	k1     = 1.2
	bParam = 0.75

	weightName = 3.0
	weightTags = 2.5
	weightDesc = 2.0
	weightBody = 1.0
)

type posting struct {
	doc int
	tf  float64
}

type docEntry struct {
	fact *Fact
	len  float64
}

// index supports incremental upsert: Save replaces one document's
// postings in place (or appends a new one) instead of rebuilding
// everything; the full build path stays for rescans and deletes, where
// wholesale reconstruction is still the cheapest correct move — at our
// scale (thousands of facts) it is a millisecond with zero invalidation
// bugs.
type index struct {
	docs     []docEntry
	terms    map[string][]posting
	avgLen   float64
	total    float64        // sum of doc lengths; kept hot so upsert can re-derive avgLen
	docByKey map[string]int // factKey -> doc slot (scope + lowercased name)
	docTerm  [][]string     // per-doc term list, so a replace can drop exactly its own postings
}

// factKey is the index key for one fact: its scope plus lowercased name.
func factKey(f *Fact) string {
	return f.Scope + "\x00" + strings.ToLower(f.Name)
}

// docTerms splits one fact into weighted term frequencies (name 3.0,
// tags 2.5, description 2.0, body 1.0) and its document length.
func docTerms(f *Fact) (map[string]float64, float64) {
	tfs := map[string]float64{}
	addField := func(text string, w float64) {
		for _, t := range tokenize(text) {
			tfs[t] += w
		}
	}
	addField(f.Name, weightName)
	addField(strings.Join(f.Tags, " "), weightTags)
	addField(f.Description, weightDesc)
	addField(f.Body, weightBody)
	var dl float64
	for _, tf := range tfs {
		dl += tf
	}
	return tfs, dl
}

// termKeys returns the map's terms as a slice for docTerm bookkeeping.
func termKeys(tfs map[string]float64) []string {
	out := make([]string, 0, len(tfs))
	for t := range tfs {
		out = append(out, t)
	}
	return out
}

func (ix *index) build(facts []*Fact) {
	ix.docs = ix.docs[:0]
	ix.terms = make(map[string][]posting, 2048)
	ix.docByKey = make(map[string]int, len(facts))
	ix.docTerm = ix.docTerm[:0]
	var total float64
	for _, f := range facts {
		doc := len(ix.docs)
		tfs, dl := docTerms(f)
		for t, tf := range tfs {
			ix.terms[t] = append(ix.terms[t], posting{doc: doc, tf: tf})
		}
		ix.docs = append(ix.docs, docEntry{fact: f, len: dl})
		ix.docTerm = append(ix.docTerm, termKeys(tfs))
		ix.docByKey[factKey(f)] = doc
		total += dl
	}
	ix.total = total
	if len(ix.docs) > 0 {
		ix.avgLen = total / float64(len(ix.docs))
	} else {
		ix.avgLen = 1
	}
}

// upsert indexes f, replacing the existing doc for the same scope+name
// in place, or appending a new one. Only this document's postings are
// touched — the O(N) tokenize+rebuild every Save used to pay is gone.
func (ix *index) upsert(f *Fact) {
	// A zero-value index (never built) is legal: the maps and the
	// per-doc bookkeeping are created here so upsert is the only
	// initializer a caller has to know about.
	if ix.docByKey == nil {
		ix.docByKey = make(map[string]int, 64)
	}
	if ix.terms == nil {
		ix.terms = make(map[string][]posting, 64)
	}
	key := factKey(f)
	tfs, dl := docTerms(f)
	if doc, ok := ix.docByKey[key]; ok {
		ix.dropPostings(doc)
		old := ix.docs[doc]
		for t, tf := range tfs {
			ix.terms[t] = append(ix.terms[t], posting{doc: doc, tf: tf})
		}
		ix.docTerm[doc] = termKeys(tfs)
		ix.docs[doc] = docEntry{fact: f, len: dl}
		ix.total += dl - old.len
	} else {
		doc := len(ix.docs)
		for t, tf := range tfs {
			ix.terms[t] = append(ix.terms[t], posting{doc: doc, tf: tf})
		}
		ix.docs = append(ix.docs, docEntry{fact: f, len: dl})
		ix.docTerm = append(ix.docTerm, termKeys(tfs))
		ix.docByKey[key] = doc
		ix.total += dl
	}
	if len(ix.docs) > 0 {
		ix.avgLen = ix.total / float64(len(ix.docs))
	} else {
		ix.avgLen = 1
	}
}

// dropPostings removes every posting that points at doc (each doc
// contributes at most one posting per term, so the first hit per term
// is the only one).
func (ix *index) dropPostings(doc int) {
	for _, t := range ix.docTerm[doc] {
		ps := ix.terms[t]
		for i, p := range ps {
			if p.doc == doc {
				ps = append(ps[:i], ps[i+1:]...)
				break
			}
		}
		if len(ps) == 0 {
			delete(ix.terms, t)
		} else {
			ix.terms[t] = ps
		}
	}
	ix.docTerm[doc] = nil
}

// Hit is one search result.
type Hit struct {
	Fact  Fact
	Score float64
}

// search scores every document containing at least one query term and
// returns the top k (k<=0 means all), closer scopes winning ties.
func (ix *index) search(query string, k int) []Hit {
	qTerms := uniqueTokens(query)
	if len(qTerms) == 0 || len(ix.docs) == 0 {
		return nil
	}
	scores := make(map[int]float64, 64)
	n := float64(len(ix.docs))
	for _, t := range qTerms {
		ps := ix.terms[t]
		if len(ps) == 0 {
			continue
		}
		df := float64(len(ps))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range ps {
			d := ix.docs[p.doc]
			norm := k1*(1-bParam+bParam*d.len/ix.avgLen) + p.tf
			scores[p.doc] += idf * p.tf * (k1 + 1) / norm
		}
	}
	hits := make([]Hit, 0, len(scores))
	for doc, s := range scores {
		hits = append(hits, Hit{Fact: *ix.docs[doc].fact, Score: s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return scopeRank(hits[i].Fact.Scope) > scopeRank(hits[j].Fact.Scope)
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// tokenize splits text for indexing: ASCII/digit runs become words, CJK
// (and kana/hangul) runs become overlapping bigrams — the standard trick
// that gives Chinese/Japanese/Korean substring recall without a segmenter.
// Everything is lowercased.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var word strings.Builder
	var cjk []rune
	flushWord := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			out = append(out, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			flushCJK()
			word.WriteRune(r)
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func uniqueTokens(s string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range tokenize(s) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // unified ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // ext A
		(r >= 0xF900 && r <= 0xFAFF) || // compat
		(r >= 0x3040 && r <= 0x30FF) || // kana
		(r >= 0xAC00 && r <= 0xD7AF) // hangul
}
