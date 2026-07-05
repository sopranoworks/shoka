package vectorindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAndOpen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")

	st, err := Create(p, "ns", "proj", "text-embedding-3-small", 1536)
	require.NoError(t, err)
	defer st.Close()

	m, err := st.Model()
	require.NoError(t, err)
	assert.Equal(t, "text-embedding-3-small", m)

	d, err := st.Dimensions()
	require.NoError(t, err)
	assert.Equal(t, "1536", d)

	// Close and reopen
	require.NoError(t, st.Close())
	st2, err := Open(p)
	require.NoError(t, err)
	defer st2.Close()

	m2, _ := st2.Model()
	assert.Equal(t, "text-embedding-3-small", m2)
}

func TestOpenNotFound(t *testing.T) {
	_, err := Open("/nonexistent/path.db")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPutGetDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")
	st, err := Create(p, "ns", "proj", "model-a", 3)
	require.NoError(t, err)
	defer st.Close()

	vec := []float64{0.1, 0.2, 0.3}
	require.NoError(t, st.Put("docs/readme.md", vec))

	got, found, err := st.Get("docs/readme.md")
	require.NoError(t, err)
	assert.True(t, found)
	assert.InDeltaSlice(t, vec, got, 1e-10)

	// Not found
	_, found, err = st.Get("nonexistent.md")
	require.NoError(t, err)
	assert.False(t, found)

	// Delete
	require.NoError(t, st.Delete("docs/readme.md"))
	_, found, err = st.Get("docs/readme.md")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCheckModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")
	st, err := Create(p, "ns", "proj", "model-a", 768)
	require.NoError(t, err)
	defer st.Close()

	assert.NoError(t, st.CheckModel("model-a", 768))
	assert.ErrorIs(t, st.CheckModel("model-b", 768), ErrModelMismatch)
	assert.ErrorIs(t, st.CheckModel("model-a", 1536), ErrModelMismatch)
}

func TestKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")
	st, err := Create(p, "ns", "proj", "m", 2)
	require.NoError(t, err)
	defer st.Close()

	require.NoError(t, st.Put("b.md", []float64{1, 2}))
	require.NoError(t, st.Put("a.md", []float64{3, 4}))

	keys, err := st.Keys()
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "b.md"}, keys)
}

func TestCount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")
	st, err := Create(p, "ns", "proj", "m", 2)
	require.NoError(t, err)
	defer st.Close()

	n, err := st.Count()
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	require.NoError(t, st.Put("x.md", []float64{1, 2}))
	n, err = st.Count()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestReplaceAll(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.vector.db")
	st, err := Create(p, "ns", "proj", "m", 2)
	require.NoError(t, err)
	defer st.Close()

	require.NoError(t, st.Put("old.md", []float64{1, 2}))
	require.NoError(t, st.ReplaceAll(map[string][]float64{
		"new1.md": {3, 4},
		"new2.md": {5, 6},
	}))

	// old.md should be gone
	_, found, _ := st.Get("old.md")
	assert.False(t, found)

	// new entries present
	v, found, _ := st.Get("new1.md")
	assert.True(t, found)
	assert.InDeltaSlice(t, []float64{3, 4}, v, 1e-10)

	n, _ := st.Count()
	assert.Equal(t, 2, n)
}

func TestCorruptDB(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.vector.db")
	require.NoError(t, os.WriteFile(p, []byte("not a bbolt db"), 0o600))

	_, err := Open(p)
	assert.ErrorIs(t, err, ErrCorrupt)
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"docs/readme.md", "docs/readme.md"},
		{"/docs/readme.md", "docs/readme.md"},
		{"./docs/../docs/readme.md", "docs/readme.md"},
		{"", ""},
		{".", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, normalizePath(tc.in), "input: %q", tc.in)
	}
}
