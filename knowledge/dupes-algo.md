# The dupes algorithm

`internal/dupes/dupes.go`. Answers: *"which files or directories in this one snapshot are
byte-identical copies of each other?"* — and reports them at the **highest level** that is
duplicated, so a copied 40,000-file directory is one finding, not 40,000.

It is a sibling of [diff-algo.md](diff-algo.md) and shares its tree-plus-merkle idea, but
it is a separate implementation with its own `Node` type. The two have drifted; changes to
one do not propagate to the other.

## Pipeline

`FindDuplicates(files map[string]models.FileRecord, minSize int64, rootPath string)`

1. **Build the tree** from the map. Leaves get `Size` and a `Hash` chosen SHA-1-first,
   MD5-fallback (so legacy MD5-only snapshots work).
2. **`computeMetadata`** post-order: a directory's `Size` is the sum of its children's,
   and its `Hash` is the same synthetic merkle string as diff uses —
   `sort(join(child.Name+":"+child.Hash, ","))`. Same two defects as in diff: it is not
   injective (`:` / `,` unescaped), and it grows with subtree size.
3. **`indexNodes`**: `hash -> []*Node` over every node with a non-empty hash.
4. **Candidates**: hashes with `len(nodes) >= 2` whose size is `>= minSize`.
5. **Top-down `walkDeterministic`** (children sorted by name). At each node:
   - if the node is already `covered`, skip its whole subtree;
   - if its hash is a candidate and not yet reported, emit one `DuplicateGroup` containing
     every node with that hash, mark all of them **and their entire subtrees** `covered`,
     and stop recursing;
   - otherwise recurse.

Step 5 is the "highest level" rule: because the walk is top-down, the shallowest
duplicated node wins, and marking descendants `covered` suppresses the redundant
per-file findings underneath it.

`DuplicateGroup` carries `Hash`, `Size`, sorted `Paths`, `IsDir`, and `ItemCount`
(recursive count of leaf files, via `countItems`).

## Behaviours that surprise people

- **`minSize` applies to the node's own size**, and a directory's size is the sum of its
  contents. So `--min-size 1MB` will report a duplicated directory of a thousand 2 KB
  files, but not any of those files individually. That is usually what you want; it is
  not obvious.
- **Zero-byte files are not excluded** (unlike in diff). With `--min-size 0` every empty
  file in the snapshot forms one giant duplicate group.
- **Hard links are reported as duplicates.** The scanner records each name separately
  (see [scanner.md](scanner.md)), so two names for one inode look like two copies — but
  deleting one frees no space.
- **A directory whose only content is duplicated elsewhere is reported at directory
  level**; a directory that is merely *contained* in another (a subset, not an exact
  match) is not detected at all. That is open roadmap item: *"dupes: if a directory is
  entirely contained in another, it counts as a folder dupe."* Implementing it means
  moving from hash equality to subset testing, which the current merkle string cannot
  support — another reason to switch directory hashes to a real digest plus a child set.
- The `rootPath` parameter is **unused**. Callers pass it; the function ignores it.

## Memory

`FindDuplicates` takes the whole snapshot as an in-memory map — `app/dupes.go` calls
`GetFilesForSnapshot`, which materialises every row. Combined with the merkle strings
(~326 B/file measured in the diff package), `dupes` on a multi-million-file snapshot is
the most memory-hungry command in the tool. Unlike `diff`, it was never converted to
streaming; it cannot be without changing the tree build to consume `IterateFiles`.

## Output

`app/dupes.go` sorts groups by wasted space (`Size * (len(Paths)-1)`), applies `--limit`,
and prints a per-group block with a total-reclaimable summary. It is purely a report:
**nothing is ever deleted or linked.**
