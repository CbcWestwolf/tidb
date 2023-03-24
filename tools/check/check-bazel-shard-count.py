#!/usr/bin/env python3
import os

for dirpath, dirnames, filenames in os.walk(os.getcwd()):
    oldShardCount = -1
    newShardCount = 0
    for filename in filenames:
        if filename == "BUILD.bazel":
            with open(os.path.join(dirpath, filename), "r") as f:
                bazelLines = f.readlines()
            for line in bazelLines:
                if "shard_count =" in line:
                    line = line.strip()
                    line = line[len("shard_count = "):]
                    line = line[:-len(",")]
                    oldShardCount = int(line)
                    break
        elif filename.endswith("_test.go"):
            with open(os.path.join(dirpath, filename), "r") as f:
                testLines = f.readlines()
            for line in testLines:
                if "func Test" in line and "*testing.T" in line:
                    newShardCount += 1

    if oldShardCount == -1 or oldShardCount == 50:
        continue

    if newShardCount > oldShardCount:
        print(dirpath, oldShardCount, newShardCount)
        with open(os.path.join(dirpath, "BUILD.bazel"), 'r+') as f:
            lines = f.readlines()
            for i, line in enumerate(lines):
                if "shard_count = " in line:
                    lines[i] = "{0}= {1},\n".format(lines[i][:lines[i].index("= ")], newShardCount)
                    break
            f.seek(0)
            f.writelines(lines)
            f.truncate()
