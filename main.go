package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var input, output, configPath string
	var noBg, watch bool

	flag.StringVar(&input, "i", "", "Input file (.note or .mark) or directory")
	flag.StringVar(&input, "input", "", "Input file (.note or .mark) or directory")
	flag.StringVar(&output, "o", "", "Output file (.pdf) or directory")
	flag.StringVar(&output, "output", "", "Output file (.pdf) or directory")
	flag.BoolVar(&noBg, "no-bg", false, "Exclude the background layer from the PDF output")
	flag.StringVar(&configPath, "config", "config.toml", "Path to config file (TOML)")
	flag.BoolVar(&watch, "watch", false, "Run as daemon, watching directories from config [watch] section")
	flag.Parse()

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	if watch {
		if cfg.Watch.Location == "" {
			fmt.Fprintln(os.Stderr, "Error: [watch] location must be set in config for --watch mode")
			os.Exit(1)
		}
		if len(cfg.Watch.InputDirs()) == 0 {
			fmt.Fprintln(os.Stderr, "Error: [watch] requires at least one of supernote_private_cloud or webdav in config")
			os.Exit(1)
		}
		if err := runWatchMode(cfg, noBg); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if input == "" || output == "" {
		fmt.Fprintln(os.Stderr, "Usage: GoSNare -i <input> -o <output> [--no-bg] [--config config.toml]")
		fmt.Fprintln(os.Stderr, "       GoSNare --watch [--no-bg] [--config config.toml]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	info, err := os.Stat(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: input path '%s' does not exist.\n", input)
		os.Exit(1)
	}

	if info.IsDir() {
		err = processDirectory(input, output, noBg, cfg)
	} else {
		err = processSingleFile(input, output, noBg, cfg)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processSingleFile(inputFile, outputFile string, noBg bool, cfg *Config) error {
	isMark := strings.HasSuffix(inputFile, ".mark")
	isNote := strings.HasSuffix(inputFile, ".note")

	if !isMark && !isNote {
		return fmt.Errorf("input file '%s' must have a .note or .mark extension", inputFile)
	}
	if info, err := os.Stat(outputFile); err == nil && info.IsDir() {
		return fmt.Errorf("input is a file, but output '%s' is a directory; specify an output file path", outputFile)
	}
	if !strings.HasSuffix(outputFile, ".pdf") {
		return fmt.Errorf("output file '%s' must have a .pdf extension", outputFile)
	}

	if dir := filepath.Dir(outputFile); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	if isMark {
		companionPDF := strings.TrimSuffix(inputFile, ".mark")
		if _, err := os.Stat(companionPDF); err != nil {
			return fmt.Errorf("companion PDF '%s' not found for mark file '%s'", companionPDF, inputFile)
		}

		if isMarkUpToDate(inputFile, companionPDF, outputFile) {
			fmt.Printf("'%s' is already up-to-date. Skipping.\n", outputFile)
			return nil
		}

		fmt.Println("Converting mark file...")
		start := time.Now()

		if err := ConvertMarkToPDFVector(inputFile, companionPDF, outputFile, true, cfg); err != nil {
			return err
		}

		fmt.Printf("Successfully converted '%s' to '%s' in %.2fs\n", inputFile, outputFile, time.Since(start).Seconds())
		return nil
	}

	if isUpToDate(inputFile, outputFile) {
		fmt.Printf("'%s' is already up-to-date. Skipping.\n", outputFile)
		return nil
	}

	fmt.Println("Converting single file...")
	start := time.Now()

	if err := ConvertNoteToPDFVector(inputFile, outputFile, noBg, true, cfg); err != nil {
		return err
	}

	fmt.Printf("Successfully converted '%s' to '%s' in %.2fs\n", inputFile, outputFile, time.Since(start).Seconds())
	return nil
}

type convJob struct {
	input        string
	output       string
	companionPDF string
}

func processDirectory(inputDir, outputDir string, noBg bool, cfg *Config) error {
	if info, err := os.Stat(outputDir); err == nil && !info.IsDir() {
		return fmt.Errorf("input is a directory, but output '%s' is a file; specify an output directory", outputDir)
	}

	fmt.Printf("Scanning for .note and .mark files in '%s'...\n", inputDir)

	var jobs []convJob
	var numSkipped int

	err := filepath.WalkDir(inputDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".note") {
			rel, _ := filepath.Rel(inputDir, path)
			out := filepath.Join(outputDir, strings.TrimSuffix(rel, ".note")+".pdf")
			if isUpToDate(path, out) {
				numSkipped++
			} else {
				jobs = append(jobs, convJob{input: path, output: out})
			}
		} else if strings.HasSuffix(path, ".mark") {
			companionPDF := strings.TrimSuffix(path, ".mark")
			if _, err := os.Stat(companionPDF); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: companion PDF not found for '%s', skipping.\n", path)
				return nil
			}
			rel, _ := filepath.Rel(inputDir, path)
			out := filepath.Join(outputDir, strings.TrimSuffix(rel, ".mark"))
			if isMarkUpToDate(path, companionPDF, out) {
				numSkipped++
			} else {
				jobs = append(jobs, convJob{input: path, output: out, companionPDF: companionPDF})
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if len(jobs) == 0 && numSkipped == 0 {
		fmt.Println("No .note or .mark files found. Exiting.")
		return nil
	}

	if len(jobs) == 0 {
		fmt.Printf("All %d files are already up-to-date. Nothing to do.\n", numSkipped)
		return nil
	}

	fmt.Printf("Found %d modified files to convert (%d up-to-date, skipped).\n", len(jobs), numSkipped)
	start := time.Now()

	var (
		completed atomic.Int64
		wg        sync.WaitGroup
	)
	total := int64(len(jobs))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	errCh := make(chan string, len(jobs))

	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if dir := filepath.Dir(j.output); dir != "." {
				if err := os.MkdirAll(dir, 0755); err != nil {
					errCh <- fmt.Sprintf("failed to create directory '%s': %v", dir, err)
					return
				}
			}
			var err error
			if j.companionPDF != "" {
				err = ConvertMarkToPDFVector(j.input, j.companionPDF, j.output, false, cfg)
			} else {
				err = ConvertNoteToPDFVector(j.input, j.output, noBg, false, cfg)
			}
			if err != nil {
				errCh <- fmt.Sprintf("failed to convert '%s': %v", j.input, err)
			}
			n := completed.Add(1)
			fmt.Printf("\r[%d/%d] Converted %s", n, total, filepath.Base(j.input))
		}()
	}
	wg.Wait()
	close(errCh)

	fmt.Println()
	for msg := range errCh {
		fmt.Fprintln(os.Stderr, msg)
	}

	fmt.Printf("Converted %d files in %.2fs\n", len(jobs), time.Since(start).Seconds())
	return nil
}

func isUpToDate(input, output string) bool {
	outInfo, err := os.Stat(output)
	if err != nil {
		return false
	}
	inInfo, err := os.Stat(input)
	if err != nil {
		return false
	}
	return !outInfo.ModTime().Before(inInfo.ModTime())
}

func isMarkUpToDate(markPath, companionPDF, output string) bool {
	outInfo, err := os.Stat(output)
	if err != nil {
		return false
	}
	markInfo, err := os.Stat(markPath)
	if err != nil {
		return false
	}
	pdfInfo, err := os.Stat(companionPDF)
	if err != nil {
		return false
	}
	return !outInfo.ModTime().Before(markInfo.ModTime()) && !outInfo.ModTime().Before(pdfInfo.ModTime())
}
