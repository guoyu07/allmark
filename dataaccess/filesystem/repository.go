// Copyright 2013 Andreas Koch. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filesystem

import (
	"fmt"
	"github.com/andreaskoch/allmark2/common/config"
	"github.com/andreaskoch/allmark2/common/content"
	"github.com/andreaskoch/allmark2/common/logger"
	"github.com/andreaskoch/allmark2/common/route"
	"github.com/andreaskoch/allmark2/common/util/fsutil"
	"github.com/andreaskoch/allmark2/dataaccess"
	"github.com/andreaskoch/go-fswatch"
	"io/ioutil"
	"path/filepath"
	"strings"
)

var numberOfWatchers int

type Repository struct {
	logger    logger.Logger
	hash      string
	directory string
	watcher   *watcherFactory

	newItem     chan *dataaccess.RepositoryEvent // new items which are discovered after the first index has been built
	changedItem chan *dataaccess.RepositoryEvent // items with changed content
	movedItem   chan *dataaccess.RepositoryEvent // items which moved
}

func NewRepository(logger logger.Logger, directory string) (*Repository, error) {

	// check if path exists
	if !fsutil.PathExists(directory) {
		return nil, fmt.Errorf("The path %q does not exist.", directory)
	}

	// check if the supplied path is a file
	if isDirectory, _ := fsutil.IsDirectory(directory); !isDirectory {
		directory = filepath.Dir(directory)
	}

	// abort if the supplied path is a reserved directory
	if isReservedDirectory(directory) {
		return nil, fmt.Errorf("The path %q is using a reserved name and cannot be a root.", directory)
	}

	// hash provider: use the directory name for the hash (for now)
	directoryName := strings.ToLower(filepath.Base(directory))
	hash, err := getStringHash(directoryName)
	if err != nil {
		return nil, fmt.Errorf("Cannot create a hash for the repository with the name %q. Error: %s", directoryName, err)
	}

	return &Repository{
		logger:    logger,
		directory: directory,
		hash:      hash,
		watcher:   newWatcherFactory(logger),

		newItem:     make(chan *dataaccess.RepositoryEvent, 1),
		changedItem: make(chan *dataaccess.RepositoryEvent, 1),
		movedItem:   make(chan *dataaccess.RepositoryEvent, 1),
	}, nil
}

func (repository *Repository) InitialItems() chan *dataaccess.RepositoryEvent {

	// open the channel
	startupItem := make(chan *dataaccess.RepositoryEvent, 1)

	go func() {

		// repository directory item
		repository.discoverItems(repository.Path(), startupItem)

		// close the channel. All items have been indexed
		close(startupItem)
	}()

	return startupItem
}

func (repository *Repository) NewItems() chan *dataaccess.RepositoryEvent {
	return repository.newItem
}

func (repository *Repository) ChangedItems() chan *dataaccess.RepositoryEvent {
	return repository.changedItem
}

func (repository *Repository) MovedItems() chan *dataaccess.RepositoryEvent {
	return repository.movedItem
}

func (repository *Repository) Id() string {
	return repository.hash
}

func (repository *Repository) Path() string {
	return repository.directory
}

// Create a new Item for the specified path.
func (repository *Repository) discoverItems(itemPath string, targetChannel chan *dataaccess.RepositoryEvent) {

	repository.logger.Debug("# folder watchers: %v", fswatch.NumberOfFolderWatchers())

	// abort if path does not exist
	if !fsutil.PathExists(itemPath) {
		targetChannel <- dataaccess.NewEvent(nil, fmt.Errorf("The path %q does not exist.", itemPath))
		return
	}

	// abort if path is reserved
	if isReservedDirectory(itemPath) {
		targetChannel <- dataaccess.NewEvent(nil, fmt.Errorf("The path %q is using a reserved name and cannot be an item.", itemPath))
		return
	}

	// make sure the item directory points to a folder not a file
	itemDirectory := itemPath
	if isDirectory, _ := fsutil.IsDirectory(itemPath); !isDirectory {
		itemDirectory = filepath.Dir(itemPath)
	}

	// create the item
	item, recurse := repository.getItemFromDirectory(repository.Path(), itemDirectory)

	// send the item to the target channel
	targetChannel <- dataaccess.NewEvent(item, nil)

	// recurse for child items
	if recurse {
		childItemDirectories := getChildDirectories(itemDirectory)
		for _, childItemDirectory := range childItemDirectories {
			repository.discoverItems(childItemDirectory, targetChannel)
		}
	}
}

func (repository *Repository) getItemFromDirectory(repositoryPath, itemDirectory string) (item *dataaccess.Item, recurse bool) {

	// physical item from markdown file
	if found, markdownFilePath := findMarkdownFileInDirectory(itemDirectory); found {

		// create an item from the markdown file
		return repository.newItemFromFile(repositoryPath, itemDirectory, markdownFilePath)

	}

	// virtual item
	if directoryContainsItems(itemDirectory, 1) {
		return repository.newVirtualItem(repositoryPath, itemDirectory)
	}

	// file collection item
	return repository.newFileCollectionItem(repositoryPath, itemDirectory)
}

func (repository *Repository) newItemFromFile(repositoryPath, itemDirectory, filePath string) (item *dataaccess.Item, recurse bool) {
	// route
	route, err := route.NewFromItemPath(repositoryPath, filePath)
	if err != nil {
		repository.logger.Error("Cannot create an Item for the path %q. Error: %s", itemDirectory, err)
		return
	}

	// content provider
	checkIntervalInSeconds := 2
	contentProvider := newFileContentProvider(filePath, route, checkIntervalInSeconds)

	// create the file index
	filesDirectory := filepath.Join(itemDirectory, config.FilesDirectoryName)
	files := getFiles(repositoryPath, itemDirectory, filesDirectory)

	// create the item
	item, err = dataaccess.NewItem(route, contentProvider, files)
	if err != nil {
		repository.logger.Error("Cannot create Item %q. Error: %s", route, err)
		return
	}

	if route.Level() > 0 {

		repository.watcher.SubDirectories(itemDirectory, 2, func(change *fswatch.FolderChange) {

			fmt.Println("------------physical item-------------")
			fmt.Println(itemDirectory)
			fmt.Println("------------------------------------------")

			// remove the parent item since we cannot easily determine which child has gone away
			repository.movedItem <- dataaccess.NewEvent(item, nil)

			// recreate this item
			repository.discoverItems(itemDirectory, repository.newItem)
		})

		repository.watcher.AllFiles(filesDirectory, 2, func(change *fswatch.FolderChange) {

			fmt.Println("------------physical files----------------")
			fmt.Println(itemDirectory)
			fmt.Println("------------------------------------------")

			// update file list
			repository.logger.Info("Updating the file list for item %q", item.String())
			newFiles := getFiles(repositoryPath, itemDirectory, filesDirectory)
			item.SetFiles(newFiles)

			// update the parent item
			repository.changedItem <- dataaccess.NewEvent(item, nil)
		})

	}

	// watch for content changes
	go func() {
		contentChangeChannel := item.ChangeEvent()
	ContentChannelLoop:
		for changeEvent := range contentChangeChannel {

			switch changeEvent {

			case content.TypeChanged:
				{
					repository.logger.Info("Item %q changed.", item)
					repository.changedItem <- dataaccess.NewEvent(item, nil)
				}

			case content.TypeMoved:
				{
					// remove the parent item since we cannot easily determine which child has gone away
					repository.movedItem <- dataaccess.NewEvent(item, nil)

					// recreate this item
					repository.discoverItems(itemDirectory, repository.newItem)

					break ContentChannelLoop
				}

			}

		}

		repository.logger.Debug("Exiting content listener for item %q.", item)
	}()

	return item, true
}

func (repository *Repository) newVirtualItem(repositoryPath, itemDirectory string) (item *dataaccess.Item, recurse bool) {

	title := filepath.Base(itemDirectory)
	content := fmt.Sprintf(`# %s`, title)

	// route
	route, err := route.NewFromItemDirectory(repositoryPath, itemDirectory)
	if err != nil {
		repository.logger.Error("Cannot create an Item for the path %q. Error: %s", itemDirectory, err)
		return
	}

	// content provider
	contentProvider := newTextContentProvider(content, route)

	// create the file index
	filesDirectory := filepath.Join(itemDirectory, config.FilesDirectoryName)
	files := getFiles(repositoryPath, itemDirectory, filesDirectory)

	// create the item
	item, err = dataaccess.NewItem(route, contentProvider, files)
	if err != nil {
		repository.logger.Error("Cannot create Item %q. Error: %s", route, err)
		return
	}

	repository.watcher.SubDirectories(itemDirectory, 2, func(change *fswatch.FolderChange) {

		fmt.Println("------------virtual item------------------")
		fmt.Println(itemDirectory)
		fmt.Println("------------------------------------------")

		// remove the parent item since we cannot easily determine which child has gone away
		repository.movedItem <- dataaccess.NewEvent(item, nil)

		// recreate this item
		repository.discoverItems(itemDirectory, repository.newItem)
	})

	return item, true
}

func (repository *Repository) newFileCollectionItem(repositoryPath, itemDirectory string) (item *dataaccess.Item, recurse bool) {

	title := filepath.Base(itemDirectory)
	content := fmt.Sprintf(`# %s`, title)

	// route
	route, err := route.NewFromItemDirectory(repositoryPath, itemDirectory)
	if err != nil {
		repository.logger.Error("Cannot create an Item for the path %q. Error: %s", itemDirectory, err)
		return
	}

	// content provider
	contentProvider := newTextContentProvider(content, route)

	// create the file index
	filesDirectory := itemDirectory
	files := getFiles(repositoryPath, itemDirectory, filesDirectory)

	// create the item
	item, err = dataaccess.NewItem(route, contentProvider, files)
	if err != nil {
		repository.logger.Error("Cannot create Item %q. Error: %s", route, err)
		return
	}

	repository.watcher.AllFiles(itemDirectory, 2, func(change *fswatch.FolderChange) {

		fmt.Println("-------------file item--------------------")
		fmt.Println(itemDirectory)
		fmt.Println("------------------------------------------")

		if directoryContainsItems(filesDirectory, 1) {
			// type change

			// remove the parent item since we cannot easily determine which child has gone away
			repository.movedItem <- dataaccess.NewEvent(item, nil)

			// recreate this item
			repository.discoverItems(itemDirectory, repository.newItem)

		} else {

			// update files
			newFiles := getFiles(repositoryPath, itemDirectory, filesDirectory)
			item.SetFiles(newFiles)

			// update the parent item
			repository.changedItem <- dataaccess.NewEvent(item, nil)
		}
	})

	return item, false
}

func directoryContainsItems(directory string, maxdepth int) bool {

	directoryEntries, _ := ioutil.ReadDir(directory)
	for _, entry := range directoryEntries {

		childDirectory := filepath.Join(directory, entry.Name())

		if entry.IsDir() {
			if isReservedDirectory(childDirectory) {
				continue
			}

			if maxdepth > 0 {

				// recurse
				if directoryContainsItems(childDirectory, maxdepth-1) {
					return true
				}
			}

			continue
		}

		if isMarkdownFile(childDirectory) {
			return true
		}

		continue
	}

	return false
}
