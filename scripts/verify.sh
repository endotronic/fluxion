#!/bin/bash
set -e

# Setup
TEST_DIR="tmp_verify_data"
DB_FILE="verify.db"
rm -rf "$TEST_DIR" "${TEST_DIR}_moved" "$DB_FILE"
mkdir -p "$TEST_DIR"

echo "Creating initial files..."
echo "Hello World" > "$TEST_DIR/file1.txt"
echo "Data" > "$TEST_DIR/file2.txt"

echo "Running 1st snapshot..."
./fluxion s --db "$DB_FILE" --name "Initial" "$TEST_DIR"

echo "Verifying list..."
./fluxion l --db "$DB_FILE"

echo "Modifying files..."
echo "Hello Modified" > "$TEST_DIR/file1.txt"
echo "New File" > "$TEST_DIR/file3.txt"

echo "Running 2nd snapshot..."
./fluxion s --db "$DB_FILE" --name "Modified" "$TEST_DIR"

echo "Running diff..."
DIFF_OUT=$(./fluxion d --db "$DB_FILE" 1 2)
echo "$DIFF_OUT"
if [[ "$DIFF_OUT" != *"[M] "*"file1.txt"* ]]; then
  echo "Error: Diff failed to detect modification."
  exit 1
fi
if [[ "$DIFF_OUT" != *"[+] "*"file3.txt"* ]]; then
  echo "Error: Diff failed to detect addition."
  exit 1
fi

echo "Testing Merge..."
./fluxion m --db "$DB_FILE" --name "MergedSnap" 1 2
./fluxion l --db "$DB_FILE"

echo "Testing Dupes..."
cp "$TEST_DIR/file3.txt" "$TEST_DIR/file3_copy.txt"
./fluxion s --db "$DB_FILE" --name "WithDupes" "$TEST_DIR"
DUPES_OUT=$(./fluxion dupes --db "$DB_FILE" --min-size 1 "WithDupes")
echo "$DUPES_OUT"
if [[ "$DUPES_OUT" != *"file3.txt"* ]]; then
    echo "Error: Dupes failed to detect duplicate."
    exit 1
fi

echo "Testing Relative Path Move..."
# Move directory to simulate mount point change
mv "$TEST_DIR" "${TEST_DIR}_moved"
./fluxion s --db "$DB_FILE" --name "MovedRoot" "${TEST_DIR}_moved"
# Diff snapshot "WithDupes" vs snapshot "MovedRoot"
# Contents are identical (including the duplicate), just different root. Should be no diff.
DIFF_MOVE=$(./fluxion d --db "$DB_FILE" "WithDupes" "MovedRoot")
echo "$DIFF_MOVE"
if [[ "$DIFF_MOVE" != *"No differences found"* ]]; then
    echo "Error: Relative path move check failed."
    exit 1
fi

# Test Exclude Option
echo "Testing Exclude Option..."
# Restore directory to original location
rm -rf "$TEST_DIR"
mv "${TEST_DIR}_moved" "$TEST_DIR"

# Modify a file in a subdirectory
mkdir -p "$TEST_DIR/exclude_me"
echo "I should be ignored" > "$TEST_DIR/exclude_me/ignored.txt"
echo "I should be seen" > "$TEST_DIR/safe_dir.txt"

./fluxion s --db "$DB_FILE" --name "ExcludeTest" "$TEST_DIR"

# Diff against "WithDupes" (which doesn't have these new files)
# Without exclude, we should see both added
DIFF_FULL=$(./fluxion d --db "$DB_FILE" "WithDupes" "ExcludeTest")
# Check for exclude_me dir or file
if [[ "$DIFF_FULL" != *"exclude_me"* ]]; then
    echo "Error: Diff failed to find file without exclude."
    exit 1
fi

# With exclude, exclude_me should be gone, but safe_dir.txt should remain
DIFF_EXCL=$(./fluxion d --db "$DB_FILE" --exclude "exclude_me" "WithDupes" "ExcludeTest")

if [[ "$DIFF_EXCL" == *"exclude_me"* ]]; then
    echo "Error: Exclude failed! Found ignored file/dir."
    exit 1
fi

if [[ "$DIFF_EXCL" != *"[+] "*"safe_dir.txt"* ]]; then
    echo "Error: Exclude was too aggressive! Missing safe file."
    exit 1
fi

# Test Coverage — the delete predicate. Content-only, path-insensitive, and the exit
# status is the answer, so it is checked here rather than just the output text.
echo "Testing Coverage..."
COV_DIR="${TEST_DIR}_cov"
rm -rf "$COV_DIR"
mkdir -p "$COV_DIR/somewhere/else"
# Same content as two files in TEST_DIR, deliberately at unrelated paths.
cp "$TEST_DIR/file1.txt" "$COV_DIR/somewhere/else/renamed_1.dat"
cp "$TEST_DIR/safe_dir.txt" "$COV_DIR/renamed_2.dat"
./fluxion s --db "$DB_FILE" --name "CovKeeper" --dir "$COV_DIR" > /dev/null

# Every file of CovKeeper exists by content in ExcludeTest, at different paths. Exit 0.
if ! ./fluxion coverage --db "$DB_FILE" "CovKeeper" "ExcludeTest" > /dev/null; then
    echo "Error: Coverage reported a loss for content that is present at another path."
    exit 1
fi

# Add one file that exists nowhere else. Must be reported, and must exit 2.
echo "only copy in the world" > "$COV_DIR/unique.txt"
./fluxion s --db "$DB_FILE" --name "CovUnique" --dir "$COV_DIR" > /dev/null
set +e
COV_OUT=$(./fluxion coverage --db "$DB_FILE" "CovUnique" "ExcludeTest")
COV_RC=$?
set -e
echo "$COV_OUT"

if [[ "$COV_RC" -ne 2 ]]; then
    echo "Error: Coverage exited $COV_RC, expected 2 for an uncovered file."
    exit 1
fi

if [[ "$COV_OUT" != *"unique.txt"* ]]; then
    echo "Error: Coverage did not name the file that would be lost."
    exit 1
fi

if [[ "$COV_OUT" == *"renamed_1.dat"* ]]; then
    echo "Error: Coverage reported a file whose content is present at another path."
    exit 1
fi

# Cleanup
rm -rf "$TEST_DIR" "${TEST_DIR}_moved" "$COV_DIR" "$DB_FILE"
echo
echo "Verification passed!"
