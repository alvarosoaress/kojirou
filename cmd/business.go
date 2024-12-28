package cmd

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"sort"
	"sync"

	"github.com/leotaku/kojirou/cmd/crop"
	"github.com/leotaku/kojirou/cmd/filter"
	"github.com/leotaku/kojirou/cmd/formats"
	"github.com/leotaku/kojirou/cmd/formats/disk"
	"github.com/leotaku/kojirou/cmd/formats/download"
	"github.com/leotaku/kojirou/cmd/formats/kindle"
	"github.com/leotaku/kojirou/cmd/split"
	md "github.com/leotaku/kojirou/mangadex"
	"golang.org/x/text/language"
)

func run() error {
	manga, err := download.MangadexSkeleton(identifierArg)
	if err != nil {
		return fmt.Errorf("skeleton: %w", err)
	}

	chapters, err := getChapters(*manga)
	if err != nil {
		return fmt.Errorf("chapters: %w", err)
	}
	*manga = manga.WithChapters(chapters)

	formats.PrintSummary(manga)
	if dryRunArg {
		return nil
	}

	covers, err := getCovers(manga)
	if err != nil {
		return fmt.Errorf("covers: %w", err)
	}
	*manga = manga.WithCovers(covers)

	dir := kindle.NewNormalizedDirectory(outArg, manga.Info.Title, kindleFolderModeArg)
	for _, volume := range manga.Sorted() {
		if err := handleVolume(*manga, volume, dir); err != nil {
			return fmt.Errorf("volume %v: %w", volume.Info.Identifier, err)
		}
	}

	return nil
}

func handleVolume(skeleton md.Manga, volume md.Volume, dir kindle.NormalizedDirectory) error {
	p := formats.TitledProgress(fmt.Sprintf("Volume: %v", volume.Info.Identifier))
	if dir.Has(volume.Info.Identifier) && !forceArg {
		p.Cancel("Skipped")
		return nil
	}

	pages, err := getPages(volume, p)
	if err != nil {
		return fmt.Errorf("pages: %w", err)
	}

	if autocropArg {
		if err := autoCrop(pages); err != nil {
			return fmt.Errorf("autocrop: %w", err)
		}
	}
	
	if rotateAndSplitArg {
		if pages, err = rotateAndSplit(pages); err != nil {
			return fmt.Errorf("rotateAndSplit: %w", err)
		}
	}

	if rotateArg {
		if err := rotateDoublePage(pages); err != nil {
			return fmt.Errorf("rotateDoublePage: %w", err)
		}
	}

	// // Compress the images before processing
	// if pages, err = compressPages(pages, 50); err != nil { // Example quality set to 75
	// 	return fmt.Errorf("compressPages: %w", err)
	// }

	mangaForVolume := skeleton.WithChapters(volume.Sorted()).WithPages(pages)
	mobi := kindle.GenerateMOBI(mangaForVolume)
	mobi.RightToLeft = !leftToRightArg
	mobi.Title = fmt.Sprintf("%v: %v",
		skeleton.Info.Title,
		volume.Info.Identifier.StringFilled(fillVolumeNumberArg, 0, false),
	)

	p = formats.VanishingProgress("Writing...")
	if err := dir.Write(volume.Info.Identifier, mobi, p); err != nil {
		p.Cancel("Error")
		return fmt.Errorf("write: %w", err)
	}
	p.Done()

	return nil
}

func getChapters(manga md.Manga) (md.ChapterList, error) {
	chapters, err := download.MangadexChapters(identifierArg)
	if err != nil {
		return nil, fmt.Errorf("mangadex: %w", err)
	}

	if diskArg != "" {
		p := formats.VanishingProgress("Disk...")
		diskChapters, err := disk.LoadChapters(diskArg, language.Make(languageArg), p)
		if err != nil {
			p.Cancel("Error")
			return nil, fmt.Errorf("disk: %w", err)
		}
		p.Done()
		chapters = append(chapters, diskChapters...)
	}

	chapters, err = filterAndSortFromFlags(chapters)
	if err != nil {
		return nil, fmt.Errorf("filter: %w", err)
	}

	// Ensure chapters from disk are preferred
	if diskArg != "" {
		chapters = chapters.SortBy(func(a md.ChapterInfo, b md.ChapterInfo) bool {
			return a.GroupNames.String() == "Filesystem" && b.GroupNames.String() != "Filesystem"
		})
	}

	return filter.RemoveDuplicates(chapters), nil
}

func getCovers(manga *md.Manga) (md.ImageList, error) {
	p := formats.VanishingProgress("Covers")
	covers, err := download.MangadexCovers(manga, p)
	if err != nil {
		p.Cancel("Error")
		return nil, fmt.Errorf("mangadex: %w", err)
	}
	p.Done()

	// Covers from disk should automatically be preferred, because
	// they appear later in the list and thus should override the
	// earlier downloaded covers.
	if diskArg != "" {
		p := formats.VanishingProgress("Disk...")
		diskCovers, err := disk.LoadCovers(diskArg, p)
		if err != nil {
			p.Cancel("Error")
			return nil, fmt.Errorf("disk: %w", err)
		}
		p.Done()
		covers = append(covers, diskCovers...)
	}

	return covers, nil
}
func getPages(volume md.Volume, p formats.CliProgress) (md.ImageList, error) {
    var wg sync.WaitGroup
    var mu sync.Mutex
    var combinedPages md.ImageList
    var mangadexErr, diskErr error

    wg.Add(1)
    go func() {
        defer wg.Done()
        mangadexPages, err := download.MangadexPages(volume.Sorted().FilterBy(func(ci md.ChapterInfo) bool {
            return ci.GroupNames.String() != "Filesystem"
        }), dataSaverArg, p)
        if err != nil {
            p.Cancel("Error")
            mangadexErr = fmt.Errorf("mangadex: %w", err)
            return
        }
        mu.Lock()
        combinedPages = append(combinedPages, mangadexPages...)
        mu.Unlock()
    }()

    wg.Add(1)
    go func() {
        defer wg.Done()
        diskPages, err := disk.LoadPages(volume.Sorted().FilterBy(func(ci md.ChapterInfo) bool {
            return ci.GroupNames.String() == "Filesystem"
        }), p)
        if err != nil {
            p.Cancel("Error")
            diskErr = fmt.Errorf("disk: %w", err)
            return
        }
        mu.Lock()
        combinedPages = append(combinedPages, diskPages...)
        mu.Unlock()
    }()

    wg.Wait()
    if mangadexErr != nil {
        return nil, mangadexErr
    }
    if diskErr != nil {
        return nil, diskErr
    }

    p.Done()
    return combinedPages, nil
}

func autoCrop(pages md.ImageList) error {
	p := formats.VanishingProgress("Cropping..")
	p.Increase(len(pages))

	for i, page := range pages {
		if cropped, err := crop.Crop(pages[i].Image, crop.Limited(pages[i].Image, 0.1)); err != nil {
			p.Cancel("Error")
			return fmt.Errorf("chapter %v: page %v: %w", page.ChapterIdentifier, page.ImageIdentifier, err)
		} else {
			pages[i].Image = cropped
			p.Add(1)
		}
	}
	p.Done()

	return nil
}

func filterAndSortFromFlags(cl md.ChapterList) (md.ChapterList, error) {
	if languageArg != "" {
		lang := language.Make(languageArg)
		cl = filter.FilterByLanguage(cl, lang)
	}
	if groupsFilter != "" {
		cl = filter.FilterByRegex(cl, "GroupNames", groupsFilter)
	}
	if volumesFilter != "" {
		ranges := filter.ParseRanges(volumesFilter)
		cl = filter.FilterByIdentifier(cl, "VolumeIdentifier", ranges)
	}
	if chaptersFilter != "" {
		ranges := filter.ParseRanges(chaptersFilter)
		cl = filter.FilterByIdentifier(cl, "Identifier", ranges)
	}

	switch rankArg {
	case "newest":
		cl = filter.SortByNewest(cl)
	case "newest-total":
		cl = filter.SortByNewestGroup(cl)
	case "views":
		cl = filter.SortByViews(cl)
	case "views-total":
		cl = filter.SortByGroupViews(cl)
	case "most":
		cl = filter.SortByMost(cl)
	default:
		return nil, fmt.Errorf(`not a valid ranking algorithm: "%v"`, rankArg)
	}

	return cl, nil
}

func rotateDoublePage(pages md.ImageList) error {
	p := formats.VanishingProgress("Rotating..")
	p.Increase(len(pages))

	sort.Slice(pages, func(i, j int) bool {
		return pages[i].ImageIdentifier < pages[j].ImageIdentifier
	})

	for i, page := range pages {
		if split.IsDoublePage(page.Image) {
			landscapeImage, _ := split.RotateImage(page.Image)
			pages[i].Image = landscapeImage
		} 
		p.Add(1)
	}

	p.Done()
	return nil
}

func rotateAndSplit(pages md.ImageList) (md.ImageList, error) {
    p := formats.VanishingProgress("Splitting..")
    p.Increase(len(pages))

    sort.Slice(pages, func(i, j int) bool {
        return pages[i].ImageIdentifier < pages[j].ImageIdentifier
    })

    occupied := make(map[int]bool)
    newPages := make(md.ImageList, len(pages))
    copy(newPages, pages)

    gammaValue := gammaArg

    for i, page := range pages {
        imgId := split.GetNextImageIdentifier(page.ImageIdentifier, occupied)
        image := page.Image

        adjustedImage, err := AdjustGamma(image, gammaValue)
        if err != nil {
            return nil, err
        }

        if split.IsDoublePage(adjustedImage) {
            landscapeImage, _ := split.RotateImage(adjustedImage)
            newPages[i].Image = landscapeImage
            newPages[i].ImageIdentifier = imgId

            leftImage, rightImage, _ := split.SplitVertically(adjustedImage)

            rightPage := md.Image{
                Image:             rightImage,
                ChapterIdentifier: page.ChapterIdentifier,
                VolumeIdentifier:  page.VolumeIdentifier,
                ImageIdentifier:   imgId + 1,
            }

            leftPage := md.Image{
                Image:             leftImage,
                ChapterIdentifier: page.ChapterIdentifier,
                VolumeIdentifier:  page.VolumeIdentifier,
                ImageIdentifier:   imgId + 2,
            }

            newPages = append(newPages, rightPage, leftPage)
            occupied[rightPage.ImageIdentifier] = true
            occupied[leftPage.ImageIdentifier] = true
        } else {
            newPages[i].Image = adjustedImage
            newPages[i].ImageIdentifier = imgId
        }

        occupied[imgId] = true
        p.Add(1)
    }

    p.Done()
    return newPages, nil
}


func AdjustGamma(img image.Image, gamma float64) (image.Image, error) {
    if gamma <= 0 {
        return nil, fmt.Errorf("gamma must be greater than 0")
    }

	if gamma == 1 {
		return img, nil
	}

    gammaLUT := make([]uint8, 256)
    for i := 0; i < 256; i++ {
        gammaLUT[i] = uint8(math.Min(255, math.Max(0, math.Pow(float64(i)/255.0, gamma)*255.0)))
    }

    bounds := img.Bounds()
    adjustedImg := image.NewRGBA(bounds)

    numGoroutines := 4
    var wg sync.WaitGroup
    wg.Add(numGoroutines)

    processRange := func(startY, endY int) {
        defer wg.Done()
        for y := startY; y < endY; y++ {
            for x := bounds.Min.X; x < bounds.Max.X; x++ {
                originalColor := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
                adjustedColor := color.RGBA{
                    R: gammaLUT[originalColor.R],
                    G: gammaLUT[originalColor.G],
                    B: gammaLUT[originalColor.B],
                    A: originalColor.A,
                }
                adjustedImg.SetRGBA(x, y, adjustedColor)
            }
        }
    }

    height := bounds.Max.Y - bounds.Min.Y
    chunkSize := height / numGoroutines
    for i := 0; i < numGoroutines; i++ {
        startY := bounds.Min.Y + i*chunkSize
        endY := startY + chunkSize
        if i == numGoroutines-1 {
            endY = bounds.Max.Y
        }
        go processRange(startY, endY)
    }

    wg.Wait()

    return adjustedImg, nil
}

// CompressImage compresses an image using JPEG format with the specified quality.
func CompressImage(img image.Image, quality int) (image.Image, error) {
	var buf bytes.Buffer
	opts := &jpeg.Options{Quality: quality}

	// Encode the image to JPEG format with the specified quality
	if err := jpeg.Encode(&buf, img, opts); err != nil {
		return nil, fmt.Errorf("failed to compress image: %w", err)
	}

	// Decode the JPEG back to image.Image
	compressedImg, err := jpeg.Decode(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to decode compressed image: %w", err)
	}

	return compressedImg, nil
}

// compressPages compresses each page in the ImageList using the specified quality.
func compressPages(pages md.ImageList, quality int) (md.ImageList, error) {
	p := formats.VanishingProgress("Compressing..")
	p.Increase(len(pages))

	var wg sync.WaitGroup
	var mu sync.Mutex
	compressedPages := make(md.ImageList, len(pages))

	wg.Add(len(pages))
	for i, page := range pages {
		go func(i int, page md.Image) {
			defer wg.Done()
			compressedImg, err := CompressImage(page.Image, quality)
			if err != nil {
				p.Cancel("Error")
				return
			}
			mu.Lock()
			compressedPages[i] = md.Image{
				Image:             compressedImg,
				ChapterIdentifier: page.ChapterIdentifier,
				VolumeIdentifier:  page.VolumeIdentifier,
				ImageIdentifier:   page.ImageIdentifier,
			}
			mu.Unlock()
			p.Add(1)
		}(i, page)
	}

	wg.Wait()
	p.Done()

	return compressedPages, nil
}
