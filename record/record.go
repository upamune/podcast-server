package record

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/go-co-op/gocron"
	"github.com/goccy/go-yaml"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/sourcegraph/conc/pool"
	"github.com/upamune/radicaster/config"
	"github.com/upamune/radicaster/ffmpeg"
	"github.com/upamune/radicaster/metadata"
	"github.com/upamune/radicaster/timeutil"
	"github.com/yyoshiki41/go-radiko"
	"github.com/yyoshiki41/radigo"
)

type Recorder struct {
	httpClient *retryablehttp.Client
	client     *radiko.Client
	logger     zerolog.Logger

	targetDir string

	scheduler struct {
		sync.RWMutex
		*gocron.Scheduler
	}

	configFilePath string
	config         struct {
		sync.RWMutex
		config.Config
	}
}

func NewRecorder(logger zerolog.Logger, client *radiko.Client, targetDir string, initConfig config.Config, configFilePath string) (*Recorder, error) {
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil
	httpClient.RequestLogHook = func(_ retryablehttp.Logger, request *http.Request, i int) {
		logger.Debug().
			Str("label", "go-retryablehttp").
			Str("method", request.Method).
			Str("url", request.URL.String()).
			Int("attempt", i).
			Msg("request_log_hook")
	}
	httpClient.ResponseLogHook = func(_ retryablehttp.Logger, response *http.Response) {
		logger.Debug().
			Str("label", "go-retryablehttp").
			Str("method", response.Request.Method).
			Str("url", response.Request.URL.String()).
			Int("status_code", response.StatusCode).
			Msg("response_log_hook")
	}
	r := &Recorder{
		client:         client,
		httpClient:     httpClient,
		logger:         logger,
		targetDir:      targetDir,
		configFilePath: configFilePath,
	}
	r.config.Config = initConfig

	if err := r.restartScheduler(); err != nil {
		return nil, errors.Wrap(err, "failed to update scheduler")
	}

	return r, nil
}

func (r *Recorder) Record(p config.Program) (err error) {
	var (
		taskStartedTime = time.Now()
		logger          = r.logger.With().Str("task_id", xid.New().String()).Logger()
	)

	logger.Info().
		Time("task_started_time", taskStartedTime).
		Msg("record task started")
	defer func() {
		taskFinishedTime := time.Now()
		logger := logger.With().
			Time("task_started_time", taskStartedTime).
			Time("task_finished_time", taskFinishedTime).
			Dur("task_duration", taskFinishedTime.Sub(taskStartedTime)).Logger()

		if err != nil {
			logger.Error().Err(err).Msg("record task finished with an error")
			return
		}
		logger.Info().Msg("record task finished")
	}()
	ctx := context.Background()
	now := time.Now().In(timeutil.JST())
	pl := pool.New().WithErrors().WithMaxGoroutines(1)
	for _, weekday := range lo.Uniq(p.Weekdays) {
		weekday := weekday
		pl.Go(func() error {
			if err := r.record(ctx, logger, now, weekday, p); err != nil {
				return errors.Wrap(err, "failed to record")
			}
			return nil
		})
	}
	if err := pl.Wait(); err != nil {
		return errors.Wrap(err, "failed to wait for all goroutines")
	}

	return nil
}

func (r *Recorder) record(ctx context.Context, logger zerolog.Logger, now time.Time, weekday timeutil.Weekday, p config.Program) error {
	logger = logger.With().Str("weekday", weekday.String()).Str("sub_task_id", xid.New().String()).Logger()

	targetDay, err := timeutil.LastSpecifiedWeekday(weekday, now)
	if err != nil {
		return errors.Wrap(err, "failed to get last specified weekday")
	}

	from, err := time.ParseInLocation(
		"200601021504",
		fmt.Sprintf("%d%02d%02d%s", targetDay.Year(), targetDay.Month(), targetDay.Day(), p.Start),
		timeutil.JST(),
	)
	if err != nil {
		return errors.Wrap(err, "failed to parse start time")
	}

	program, err := r.client.GetProgramByStartTime(ctx, p.StationID, from)
	if err != nil {
		return errors.Wrap(
			err,
			fmt.Sprintf(
				"failed to get program: station_id=%s, from=%s",
				p.StationID,
				from.Format("2006-01-02 15:04:05"),
			),
		)
	}
	logger.Info().
		Time("from", from).
		Str("program_title", program.Title).
		Str("program_ft", program.Ft).
		Str("program_to", program.To).
		Msg("program found")

	fileName := fmt.Sprintf(
		"%s_%s.%s",
		program.Title,
		from.Format("2006年01月02日"),
		p.Encoding,
	)
	output := filepath.Join(r.targetDir, fileName)

	if _, err := os.Stat(output); err == nil {
		logger.Info().Str("output", output).Msg("file already exists")
		return nil
	}

	uri, err := r.client.TimeshiftPlaylistM3U8(ctx, p.StationID, from)
	if err != nil {
		return errors.Wrap(
			err,
			fmt.Sprintf(
				"failed to get m3u8: %s %s %s",
				p.StationID,
				p.Title,
				from.Format(time.DateOnly),
			))
	}

	chunkURLs, err := radiko.GetChunklistFromM3U8(uri)
	if err != nil {
		return errors.Wrap(err, "failed to get chunklist")
	}

	aacDir, err := os.MkdirTemp(os.TempDir(), "radicaster")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(aacDir)
	logger.Debug().Str("aac_temp_dir", aacDir).Msg("created temp dir")

	if err := r.bulkDownload(chunkURLs, aacDir); err != nil {
		return errors.Wrap(err, "failed to download aac files")
	}

	logger.Info().Msg("start concating aac files")
	var concatedFile string
	if iterCount, _, err := lo.AttemptWithDelay(
		10,
		10*time.Second,
		func(i int, dur time.Duration) error {
			var err error
			logger.Info().Dur("duration", dur).Int("iter_count", i).Msg("concating aac files")
			concatedFile, err = ffmpeg.ConcatAACFilesFromList(ctx, logger, aacDir)
			if err != nil {
				logger.Error().
					Err(err).
					Str("stack", fmt.Sprintf("%+v", errors.WithStack(err))).
					Msg("failed to concat aac files")
				return errors.Wrap(err, "failed to concat aac files")
			}
			return nil
		}); err != nil {
		return errors.Wrapf(err, "failed to concat aac files after %d times", iterCount)
	}
	logger.Info().Msg("finished concating aac files")

	switch p.Encoding {
	case config.AudioFormatAAC:
		logger.Info().
			Str("output", output).
			Msg("start outputting aac")
		absPath, err := filepath.Abs(output)
		if err != nil {
			return errors.Wrap(err, "failed to get abs path")
		}
		if err := os.Rename(concatedFile, absPath); err != nil {
			return errors.Wrap(err, "failed to rename file")
		}
		logger.Info().Msg("finish outputting aac")
	case config.AudioFormatMP3:
		logger.Info().
			Str("output", output).
			Msg("start converting aac to mp3")
		if iterCount, _, err := lo.AttemptWithDelay(
			10,
			3*time.Second,
			func(i int, dur time.Duration) error {
				logger.Info().Dur("duration", dur).Int("iter_count", i).Msg("converting aac to mp3")
				if err := radigo.ConvertAACtoMP3(ctx, concatedFile, output); err != nil {
					return errors.Wrap(err, "failed to convert aac to mp3")
				}
				return nil
			}); err != nil {
			return errors.Wrapf(err, "failed to convert aac to mp3 after %d times", iterCount)
		}
		logger.Info().Msg("finish converting aac to mp3")
	default:
		return errors.Errorf("unsupported encoding: %s", p.Encoding)
	}

	if err := metadata.WriteByAudioFilePath(
		output,
		metadata.EpisodeMetadata{
			Title:       program.Title,
			Description: program.Desc,
			PublishedAt: from,
			ImageURL:    p.ImageURL,
			Path:        p.Path,
		},
	); err != nil {
		return errors.Wrap(err, "failed to write metadata")
	}
	return nil
}

func (r *Recorder) bulkDownload(urls []string, output string) error {
	p := pool.New().WithErrors()

	for i, url := range urls {
		i, url := i, url
		p.Go(func() error {
			if err := r.download(url, output); err != nil {
				return errors.Wrapf(err, "failed to download %d", i)
			}
			return nil
		})
	}
	if err := p.Wait(); err != nil {
		return errors.Wrap(err, "failed to download aac files")
	}
	return nil
}

func (r *Recorder) download(link, output string) error {
	resp, err := r.httpClient.Get(link)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, fileName := filepath.Split(link)
	file, err := os.Create(filepath.Join(output, fileName))
	if err != nil {
		return err
	}

	_, err = io.Copy(file, resp.Body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (r *Recorder) restartScheduler() error {
	s := gocron.NewScheduler(timeutil.JST())

	r.config.RLock()
	defer r.config.RUnlock()
	for _, p := range r.config.Config.Programs {
		if _, err := s.Cron(p.Cron).Do(r.Record, p); err != nil {
			return errors.Wrap(err, "failed to set cron")
		}
	}

	r.scheduler.Lock()
	defer r.scheduler.Unlock()
	if s := r.scheduler.Scheduler; s != nil {
		s.Stop()
	}
	r.scheduler.Scheduler = s
	r.scheduler.Scheduler.StartAsync()
	return nil
}

func (r *Recorder) Config() config.Config {
	r.config.RLock()
	defer r.config.RUnlock()
	return r.config.Config
}

func (r *Recorder) refreshConfig(config config.Config) (config.Config, error) {
	r.logger.Info().Msg("start refreshing config")
	defer r.logger.Info().Msg("finish refreshing config")
	r.config.Lock()
	r.config.Config = config
	r.logger.Debug().Object("config", config).Msg("config updated")
	r.config.Unlock()

	if err := r.restartScheduler(); err != nil {
		return config, errors.Wrap(err, "failed to update scheduler")
	}

	return config, nil
}

func (r *Recorder) RefreshConfig(c config.Config) (config.Config, error) {
	updatedConfig, err := r.refreshConfig(c)
	if err != nil {
		return config.Config{}, errors.Wrap(err, "failed to refresh config")
	}
	if err := r.refreshLocalConfig(updatedConfig); err != nil {
		return config.Config{}, errors.Wrap(err, "failed to refresh local config")
	}
	return updatedConfig, nil
}

func (r *Recorder) refreshLocalConfig(c config.Config) error {
	if r.configFilePath == "" {
		r.logger.Debug().
			Msg("skip refreshing local config because config file path is empty")
		return nil
	}

	f, err := os.Create(r.configFilePath)
	if err != nil {
		return errors.Wrap(err, "failed to open config file")
	}
	defer f.Close()

	if err := yaml.NewEncoder(
		f,
		yaml.Indent(2),
	).Encode(c); err != nil {
		return errors.Wrap(err, "failed to encode config")
	}
	r.logger.Debug().
		Str("config_file_path", r.configFilePath).
		Any("config", c).
		Msg("finish refreshing local config")

	return nil
}

func (r *Recorder) RefreshConfigByURL(configURL string) (config.Config, error) {
	resp, err := http.Get(configURL)
	if err != nil {
		return config.Config{}, errors.Wrap(err, "failed to get config via URL")
	}
	defer resp.Body.Close()

	config, err := config.Parse(resp.Body)
	if err != nil {
		return config, errors.Wrap(err, "failed to parse config")
	}

	return r.refreshConfig(config)
}
