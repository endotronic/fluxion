#!/bin/bash
set -e

# Setup
TEST_DIR="tmp_verify_data"
DB_FILE="verify.db"
rm -rf "$TEST_DIR" "$DB_FILE"
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
./fluxion d --db "$DB_FILE" 1 2

# Cleanup
rm -rf "$TEST_DIR" "$DB_FILE"
echo "Verification passed!"
