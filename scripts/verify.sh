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
if [[ "$DIFF_OUT" != *"[M] "*"$TEST_DIR/file1.txt"* ]]; then
  echo "Error: Diff failed to detect modification."
  exit 1
fi
if [[ "$DIFF_OUT" != *"[+] "*"$TEST_DIR/file3.txt"* ]]; then
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

# Cleanup
rm -rf "$TEST_DIR" "${TEST_DIR}_moved" "$DB_FILE"
echo "Verification passed!"
