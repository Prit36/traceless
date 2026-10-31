package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type sizeUpdate struct {
	index int
	size  int64
}

// At most four walks and eight queued completions, regardless of how many
// leftovers were found. The coordinator alone writes sizes and calls progress.
func measureContext(ctx context.Context, items []Leftover, progress func(sizeUpdate)) {
	count, single := 0, -1
	for i := range items {
		if !items[i].IsRegistry() {
			count++
			single = i
		}
	}
	if count <= 1 {
		if single >= 0 && ctx.Err() == nil {
			u := sizeUpdate{single, diskSizeContext(ctx, items[single].Path)}
			items[single].Size = u.size
			if progress != nil && ctx.Err() == nil {
				progress(u)
			}
		}
		return
	}
	jobs := make(chan int)
	done := make(chan sizeUpdate, 8)
	var wg sync.WaitGroup
	for range min(4, count) {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				u := sizeUpdate{i, diskSizeContext(ctx, items[i].Path)}
				select {
				case done <- u:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for i := range items {
			if !items[i].IsRegistry() {
				select {
				case jobs <- i:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() { wg.Wait(); close(done) }()
	for u := range done {
		items[u.index].Size = u.size
		if progress != nil && ctx.Err() == nil {
			progress(u)
		}
	}
}

// Use the sizes Windows returns with directory entries. Unlike WalkDir this
// needs no FileInfo/DirEntry objects, sorting, or per-file path allocations.
// Junctions, symlinks and other reparse points are never followed.
func diskSizeContext(ctx context.Context, path string) int64 {
	if ctx.Err() != nil {
		return 0
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return 0
	}
	if !strings.HasPrefix(path, `\\?\`) {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + path[2:]
		} else {
			path = `\\?\` + path
		}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var attr windows.Win32FileAttributeData
	if windows.GetFileAttributesEx(p, windows.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&attr))) != nil || attr.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0
	}
	if attr.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return int64(uint64(attr.FileSizeHigh)<<32 | uint64(attr.FileSizeLow))
	}
	var total int64
	stack := []string{path}
	for len(stack) > 0 && ctx.Err() == nil {
		dir := stack[len(stack)-1]
		stack[len(stack)-1] = ""
		stack = stack[:len(stack)-1]
		pattern, err := windows.UTF16PtrFromString(dir + `\*`)
		if err != nil {
			continue
		}
		var data windows.Win32finddata
		h, err := windows.FindFirstFile(pattern, &data)
		if err != nil {
			continue
		}
		for {
			if ctx.Err() != nil {
				break
			}
			if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
				if data.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
					total += int64(uint64(data.FileSizeHigh)<<32 | uint64(data.FileSizeLow))
				} else if !(data.FileName[0] == '.' && (data.FileName[1] == 0 || data.FileName[1] == '.' && data.FileName[2] == 0)) {
					stack = append(stack, dir+`\`+windows.UTF16ToString(data.FileName[:]))
				}
			}
			if windows.FindNextFile(h, &data) != nil {
				break
			}
		}
		windows.FindClose(h)
	}
	return total
}
