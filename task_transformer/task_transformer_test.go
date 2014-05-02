package task_transformer_test

import (
	"github.com/cloudfoundry-incubator/executor/file_cache"
	"github.com/cloudfoundry-incubator/executor/file_cache/fake_file_cache"
	"github.com/cloudfoundry-incubator/executor/log_streamer"
	"github.com/cloudfoundry-incubator/executor/log_streamer/fake_log_streamer"
	"github.com/cloudfoundry-incubator/executor/sequence"
	"github.com/cloudfoundry-incubator/executor/steps/download_step"
	"github.com/cloudfoundry-incubator/executor/steps/emit_progress_step"
	"github.com/cloudfoundry-incubator/executor/steps/fetch_result_step"
	"github.com/cloudfoundry-incubator/executor/steps/run_step"
	"github.com/cloudfoundry-incubator/executor/steps/try_step"
	"github.com/cloudfoundry-incubator/executor/steps/upload_step"
	. "github.com/cloudfoundry-incubator/executor/task_transformer"
	"github.com/cloudfoundry-incubator/executor/uploader"
	"github.com/cloudfoundry-incubator/executor/uploader/fake_uploader"
	"github.com/cloudfoundry-incubator/garden/client/fake_warden_client"
	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/archiver/compressor"
	"github.com/pivotal-golang/archiver/compressor/fake_compressor"
	"github.com/pivotal-golang/archiver/extractor"
	"github.com/pivotal-golang/archiver/extractor/fake_extractor"
)

var _ = Describe("TaskTransformer", func() {
	var (
		cache           file_cache.FileCache
		logger          *steno.Logger
		logStreamer     *fake_log_streamer.FakeLogStreamer
		uploader        uploader.Uploader
		extractor       extractor.Extractor
		compressor      compressor.Compressor
		wardenClient    *fake_warden_client.FakeClient
		taskTransformer *TaskTransformer
		result          string
	)

	handle := "some-container-handle"

	BeforeEach(func() {
		logStreamer = fake_log_streamer.New()
		cache = fake_file_cache.New()
		uploader = &fake_uploader.FakeUploader{}
		extractor = &fake_extractor.FakeExtractor{}
		compressor = &fake_compressor.FakeCompressor{}
		wardenClient = fake_warden_client.New()
		logger = &steno.Logger{}

		logStreamerFactory := func(models.LogConfig) log_streamer.LogStreamer {
			return logStreamer
		}

		taskTransformer = NewTaskTransformer(
			logStreamerFactory,
			cache,
			uploader,
			extractor,
			compressor,
			logger,
			"/fake/temp/dir",
		)
	})

	It("is correct", func() {
		runActionModel := models.RunAction{Script: "do-something"}
		downloadActionModel := models.DownloadAction{From: "/file/to/download"}
		uploadActionModel := models.UploadAction{From: "/file/to/upload"}
		fetchResultActionModel := models.FetchResultAction{File: "some-file"}
		tryActionModel := models.TryAction{Action: models.ExecutorAction{runActionModel}}

		emitProgressActionModel := models.EmitProgressAction{
			Action:         models.ExecutorAction{runActionModel},
			StartMessage:   "starting",
			SuccessMessage: "successing",
			FailureMessage: "failuring",
		}

		task := models.Task{
			Guid: "some-guid",
			Actions: []models.ExecutorAction{
				{runActionModel},
				{downloadActionModel},
				{uploadActionModel},
				{fetchResultActionModel},
				{tryActionModel},
				{emitProgressActionModel},
			},
			FileDescriptors: 117,
		}

		container, err := wardenClient.Create(warden.ContainerSpec{Handle: handle})
		Ω(err).ShouldNot(HaveOccurred())

		Ω(taskTransformer.StepsFor(&task, container, &result)).To(Equal([]sequence.Step{
			run_step.New(
				container,
				runActionModel,
				117,
				logStreamer,
				logger,
			),
			download_step.New(
				container,
				downloadActionModel,
				cache,
				extractor,
				"/fake/temp/dir",
				logger,
			),
			upload_step.New(
				container,
				uploadActionModel,
				uploader,
				compressor,
				"/fake/temp/dir",
				logStreamer,
				logger,
			),
			fetch_result_step.New(
				container,
				fetchResultActionModel,
				"/fake/temp/dir",
				logger,
				&result,
			),
			try_step.New(
				run_step.New(
					container,
					runActionModel,
					117,
					logStreamer,
					logger,
				),
				logger,
			),
			emit_progress_step.New(
				run_step.New(
					container,
					runActionModel,
					117,
					logStreamer,
					logger,
				),
				"starting",
				"successing",
				"failuring",
				logStreamer,
				logger,
			),
		}))
	})
})
