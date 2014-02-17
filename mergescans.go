package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os/exec"
	"path"
	"strings"
)

/*
func main() {
	flag.Parse()

	tmpdir, err := ioutil.TempDir(*tmp_dir, "")
	if err != nil {
		panic(err)
	}

	output_file := merge_scans(*front_side_file, *back_side_file, tmpdir)
	fmt.Printf(output_file)

	os.RemoveAll(tmpdir)
	if err != nil {
		panic(err)
	}
}
*/

// Issues a call to pdfserapate on the given pdf_file splitting the file into a
// series of 1-page pdf documents with the given pdf prefix. Returns the full
// paths to the separated documents.
func pdfseparate(pdf_file string, tmpdir string, prefix string) []string {
	// Separate pdf_file into one new numbered file per page.
	cmd := exec.Command(
		"pdfseparate",
		pdf_file,
		path.Join(tmpdir, prefix+"-%d.pdf"))
	err := cmd.Run()
	// pdfseparate has a dumb behavior of considering 99 an OK exit status.
	if err != nil && err.Error() != "exit status 99" {
		panic(err)
	}

	// Read the documents in the tmpdir to find out what pdfseparate just created.
	files, err := ioutil.ReadDir(tmpdir)
	if err != nil {
		panic(err)
	}

	// Assume any file matching our pdfseparate prefix is one of our outputs.
	var pages []string
	for _, file := range files {
		if strings.HasPrefix(file.Name(), prefix) {
			pages = append(pages, path.Join(tmpdir, file.Name()))
		}
	}
	return pages
}

// Merges and interleaves scanned pages from the front and back side of a
// document. Back side document is assumed to be in reverse order (last page of
// the document as the first page in the scan) due to user flipping the stack of
// documents over for the second scan.
func merge_scans(front_side_file string,
	back_side_file string,
	tmpdir string) (string, error) {

	// Separate the input documents into per-page documents.
	front_files := pdfseparate(front_side_file, tmpdir, "front")
	back_files := pdfseparate(back_side_file, tmpdir, "back")

	// Verify same number of pages.
	if len(front_files) != len(back_files) {
		return "", errors.New(
			fmt.Sprintf("Different number of front and back files: "+
				"%s (%d) and %s (%d)", front_side_file, len(front_files),
				back_side_file, len(back_files)))
	}

	// Reorder the pages to be the correct merged order
	var in_order_files []string
	for i := range front_files {
		in_order_files = append(in_order_files, front_files[i])
		// I can't believe how hard it is to reverse the order of an array.
		in_order_files = append(in_order_files, back_files[len(back_files)-1-i])
	}

	// Execute a pdfunite call to re-merge the final document
	output_file := path.Join(tmpdir, "out.pdf")
	args := in_order_files[0 : len(in_order_files)+1]
	args[len(in_order_files)] = output_file
	cmd := exec.Command("pdfunite", args...)
	err := cmd.Run()
	if err != nil {
		panic(err)
	}

	return output_file, nil
}
