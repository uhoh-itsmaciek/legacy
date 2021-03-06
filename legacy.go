package main

/*  Legacy - Simple Cassandra Backup Utility
 */
import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/goamz/goamz/aws"
	"github.com/goamz/goamz/s3"
	"github.com/iamthemovie/legacy/backup"
	"github.com/rlmcpherson/s3gof3r"
)

// LegacyArguments ...
type LegacyArguments struct {
	AwsSecret       string
	AwsAccessKey    string
	AwsRegion       string
	S3Bucket        string
	S3BasePath      string
	NewSnapshot     bool
	Keyspace        string
	DataDirectories string
	Help            bool
}

var memprofile = flag.String("memprofile", "", "write memory profile to this file")

func main() {
	go func() {
		if *memprofile != "" {
			written := false
			for {
				if written {
					os.Remove(*memprofile)
				}

				f, err := os.Create(*memprofile)
				if err != nil {
					log.Fatal(err)
				}
				pprof.WriteHeapProfile(f)
				time.Sleep(1 * time.Millisecond)
				f.Close()
			}
		}
	}()

	args, err := GetLegacyArguments()

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	if args.Help {
		flag.Usage()
		return
	}

	legacy, err := args.GetLegacy()
	if err != nil {
		log.Println(err)
		return
	}

	legacy.Run()

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.WriteHeapProfile(f)
		f.Close()
		return
	}
}

type Legacy struct {
	MachineName     string
	DataDirectories []string
	SeedSnaphshot   string
	S3Bucket        *s3.Bucket
	S3StreamBucket  *s3gof3r.Bucket
	S3BasePath      string
}

type LegacyTableManifest struct {
	SnapshotName    string
	DateCreated     string
	DateLastUpdated string
}

func (l *Legacy) Run() {
	// Every time we run, we create snapshot. This is used to check for active
	// tables / new tables. It is deleted after we've finished :)
	snapshotName, _ := CreateNewSnapshot(strconv.Itoa(int(time.Now().Unix())))
	l.SeedSnaphshot = snapshotName

	tables := l.GetTableReferences()
	for _, table := range tables {
		l.RunTableBackup(&table)
	}

	// @todo Clear specific snapshot
}

func (l *Legacy) GetManifest(tablePath string) (*LegacyTableManifest, error) {
	p := path.Join(l.S3BasePath, l.MachineName, ".legacy", tablePath, "manifest.json")
	log.Println("Getting manifest for: " + p)
	data, _ := l.S3Bucket.Get(p)
	if len(data) == 0 {
		return &LegacyTableManifest{}, errors.New("No exists?")
	}

	manifest := LegacyTableManifest{}
	json.Unmarshal(data, &manifest)
	return &manifest, nil
}

func (l *Legacy) SaveManifest(tablePath string, manifest LegacyTableManifest) {
	p := path.Join(l.S3BasePath, l.MachineName, ".legacy", tablePath, "manifest.json")
	log.Println("Saving manifest for: " + p)
	output, _ := json.Marshal(manifest)
	l.S3Bucket.Put(p, output, "application/json", s3.BucketOwnerFull, s3.Options{})
}

func (l *Legacy) RunTableBackup(table *CassandraTableMeta) {
	tableManifest, err := l.GetManifest(table.GetManifestPath())

	snapshotFileSystemPath := path.Join(table.GetDataPath(), "snapshots", l.SeedSnaphshot)
	backupFileSystemPath := path.Join(table.GetDataPath(), "backups")
	s3UploadPath := path.Join(l.S3BasePath, l.MachineName, table.GetManifestPath(), "snapshots")

	if err != nil {
		log.Println("Manifest does not exist. Computing initial snapshot upload size...")
		tableManifest = &LegacyTableManifest{
			SnapshotName:    l.SeedSnaphshot,
			DateCreated:     time.Now().Format(time.RFC3339),
			DateLastUpdated: "",
		}

		log.Println("Starting SSTable snapshot upload for table: " + table.Folder)
		log.Println("Path: " + snapshotFileSystemPath)
		backupInstance := backup.Backup{
			FileSystemRoot:    snapshotFileSystemPath,
			S3StreamBucket:    l.S3StreamBucket,
			S3Path:            path.Join(s3UploadPath, l.SeedSnaphshot),
			RemoveAfterUpload: false,
		}

		backupInstance.Run()
		l.SaveManifest(table.GetManifestPath(), *tableManifest)
		return
	}

	// Does the backup direoctory exist?
	if _, err := os.Stat(backupFileSystemPath); os.IsNotExist(err) {
		log.Println(backupFileSystemPath)
		log.Println("No backups directory present. Have incremental backups been enabled? If so, Casandra may not have flushed the SSTables yet.")
		return
	}

	backupInstance := backup.Backup{
		FileSystemRoot:    backupFileSystemPath,
		S3StreamBucket:    l.S3StreamBucket,
		S3Path:            path.Join(s3UploadPath, tableManifest.SnapshotName),
		RemoveAfterUpload: true,
	}

	backupInstance.Run()
}

func (la *LegacyArguments) GetLegacy() (*Legacy, error) {
	// Create a "TEST" snapshot in order to work out which tables are active
	// Get a list of Keyspaces and Table Names (plus directories)
	// Walk through all the directories.
	auth, _ := aws.GetAuth(
		la.AwsAccessKey,
		la.AwsSecret,
		"",
		time.Now().AddDate(0, 0, 1))

	// Check the bucket exists.
	bucket := s3.New(auth, GetAwsRegion(la.AwsRegion)).Bucket(la.S3Bucket)
	_, err := bucket.List("/", "/", "", 1)
	if err != nil {
		return nil, err
	}

	streamAccess := s3gof3r.New("", s3gof3r.Keys{
		AccessKey:     la.AwsAccessKey,
		SecretKey:     la.AwsSecret,
		SecurityToken: "",
	})

	streamBucket := streamAccess.Bucket(la.S3Bucket)

	legacy := &Legacy{DataDirectories: make([]string, 0), S3Bucket: bucket, S3StreamBucket: streamBucket}
	legacy.MachineName, _ = os.Hostname()
	for _, element := range strings.Split(la.DataDirectories, ",") {
		element = strings.TrimSpace(element)
		if len(element) == 0 {
			continue
		}

		legacy.DataDirectories = append(legacy.DataDirectories, element)
	}

	return legacy, nil
}

func (cb *Legacy) GetTableReferences() []CassandraTableMeta {
	retrieveKeyspaces := func(files []os.FileInfo, err error) []string {
		names := make([]string, 0)
		for _, element := range files {
			if !element.IsDir() {
				continue
			}

			names = append(names, element.Name())
		}

		return names
	}

	retrieveTableFolders := func(dataDir, keyspaceName string) []CassandraTableMeta {
		tableMetas := make([]CassandraTableMeta, 0)
		keyspaceFolder := path.Join(dataDir, keyspaceName)
		tableDirList, _ := ioutil.ReadDir(keyspaceFolder)
		for _, tableDir := range tableDirList {
			tableDirName := tableDir.Name()

			p := (path.Join(keyspaceFolder, tableDirName, "snapshots", cb.SeedSnaphshot))
			log.Println(p)
			if _, err := os.Stat(p); os.IsNotExist(err) {
				continue
			}

			tableMetas = append(tableMetas, CassandraTableMeta{
				Folder:        tableDirName,
				KeyspaceName:  keyspaceName,
				DataDirectory: dataDir,
			})
		}

		return tableMetas
	}

	activeTableList := []CassandraTableMeta{}
	for _, element := range cb.DataDirectories {
		// Walk through this directory and get the Keyspace
		keyspacesForDirectory := retrieveKeyspaces(ioutil.ReadDir(element))
		for _, keyspaceName := range keyspacesForDirectory {
			tables := retrieveTableFolders(element, keyspaceName)
			activeTableList = append(activeTableList, tables...)
		}
	}

	return activeTableList
}

func GetLegacyArguments() (*LegacyArguments, error) {
	args := &LegacyArguments{}
	flag.StringVar(&args.AwsSecret, "aws-secret", "", "AWS Secret")
	flag.StringVar(&args.AwsAccessKey, "aws-access-key", "", "AWS Secret Key")
	flag.StringVar(&args.AwsRegion, "aws-region", "eu-west-1", "AWS Region - Default: eu-west-1. See: http://docs.aws.amazon.com/general/latest/gr/rande.html#s3_region")
	flag.StringVar(&args.S3Bucket, "s3-bucket", "", "The name of the bucket for the backup destination.")
	flag.StringVar(&args.S3BasePath, "s3-base-path", "", "The path inside the bucket where the backups will be placed.")
	flag.StringVar(&args.Keyspace, "keyspace", "", "The Cassandra Keyspace to backup.")
	flag.StringVar(&args.DataDirectories, "directories", "/var/lib/cassandra/data", "A set of data directories that contain the keyspace / tables. For multiple, comma separate: /data1,/data2")
	flag.BoolVar(&args.Help, "help", false, "Print this info.")
	flag.BoolVar(&args.NewSnapshot, "new-snapshot", false, "Force a new snapshot.")
	flag.Parse()

	if args.Help {
		return args, nil
	}

	if len(args.AwsSecret) == 0 || len(args.AwsAccessKey) == 0 {
		return nil, errors.New("You must set both the AWS Secret and Access Key. -help for usage.")
	}

	if len(args.S3Bucket) == 0 || len(args.S3BasePath) == 0 {
		return nil, errors.New("You must set both the S3 Bucket and the Base path destination. -help for usage")
	}

	return args, nil
}

func GetAwsRegion(region string) aws.Region {
	switch region {
	case "us-gov-west-1":
		return aws.USGovWest
	case "us-east-1":
		return aws.USEast
	case "us-west-1":
		return aws.USWest
	case "us-west-2":
		return aws.USWest2
	case "eu-west-1":
		return aws.EUWest
	case "eu-central-1":
		return aws.EUCentral
	case "ap-southeast-1":
		return aws.APSoutheast
	case "ap-southeast-2":
		return aws.APSoutheast2
	case "ap-northeast-1":
		return aws.APNortheast
	case "cn-north-1":
		return aws.CNNorth
	default:
		return aws.EUWest
	}
}
