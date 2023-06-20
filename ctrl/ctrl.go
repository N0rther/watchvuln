package ctrl

import (
	"context"
	entSql "entgo.io/ent/dialect/sql"
	"fmt"
	"github.com/google/go-github/v53/github"
	"github.com/kataras/golog"
	"github.com/pkg/errors"
	"github.com/zema1/watchvuln/ent"
	"github.com/zema1/watchvuln/ent/migrate"
	"github.com/zema1/watchvuln/ent/vulninformation"
	"github.com/zema1/watchvuln/grab"
	"github.com/zema1/watchvuln/push"
	"golang.org/x/sync/errgroup"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxPageBase = 3

type WatchVulnApp struct {
	config     *WatchVulnAppConfig
	textPusher push.TextPusher
	rawPusher  push.RawPusher

	log          *golog.Logger
	db           *ent.Client
	githubClient *github.Client
	grabbers     []grab.Grabber
	prs          []*github.PullRequest
}

func NewApp(config *WatchVulnAppConfig, textPusher push.TextPusher, rawPusher push.RawPusher) (*WatchVulnApp, error) {
	drv, err := entSql.Open("sqlite3", "file:vuln_v2.sqlite3?cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, errors.Wrap(err, "failed opening connection to sqlite")
	}
	db := drv.DB()
	db.SetMaxOpenConns(1)
	dbClient := ent.NewClient(ent.Driver(drv))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	if err := dbClient.Schema.Create(ctx, migrate.WithDropIndex(true), migrate.WithDropColumn(true)); err != nil {
		return nil, errors.Wrap(err, "failed creating schema resources")
	}

	var grabs []grab.Grabber
	for _, part := range config.Sources {
		part = strings.ToLower(strings.TrimSpace(part))
		switch part {
		case "avd":
			grabs = append(grabs, grab.NewAVDCrawler())
		case "ti":
			grabs = append(grabs, grab.NewTiCrawler())
		case "oscs":
			grabs = append(grabs, grab.NewOSCSCrawler())
		case "seebug":
			grabs = append(grabs, grab.NewSeebugCrawler())
		default:
			return nil, fmt.Errorf("invalid grab source %s", part)
		}
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	githubClient := github.NewClient(&http.Client{
		Timeout:   time.Second * 5,
		Transport: tr,
	})

	return &WatchVulnApp{
		config:       config,
		textPusher:   textPusher,
		rawPusher:    rawPusher,
		log:          golog.Child("[ctrl]"),
		db:           dbClient,
		githubClient: githubClient,
		grabbers:     grabs,
	}, nil
}

func (w *WatchVulnApp) Run(ctx context.Context) error {
	w.log.Infof("initialize local database..")
	// 抓取前3页作为基准漏洞数据
	eg, initCtx := errgroup.WithContext(ctx)
	eg.SetLimit(len(w.grabbers))
	for _, grabber := range w.grabbers {
		grabber := grabber
		eg.Go(func() error {
			return w.initData(initCtx, grabber)
		})
	}
	err := eg.Wait()
	if err != nil {
		return errors.Wrap(err, "init data")
	}
	w.log.Infof("grabber finished successfully")

	localCount, err := w.db.VulnInformation.Query().Count(ctx)
	if err != nil {
		return err
	}
	w.log.Infof("system init finished, local database has %d vulns", localCount)
	if !w.config.NoStartMessage {
		providers := make([]*grab.Provider, 0, 3)
		for _, p := range w.grabbers {
			providers = append(providers, p.ProviderInfo())
		}
		msg := &push.InitialMessage{
			Version:   w.config.Version,
			VulnCount: localCount,
			Interval:  w.config.Interval.String(),
			Provider:  providers,
		}
		if err := w.textPusher.PushMarkdown("WatchVuln 初始化完成", push.RenderInitialMsg(msg)); err != nil {
			return err
		}
		if err := w.rawPusher.PushRaw(push.NewRawInitialMessage(msg)); err != nil {
			return err
		}
	}

	w.log.Infof("ticking every %s", w.config.Interval)

	defer func() {
		msg := "注意: WatchVuln 进程退出"
		if err = w.textPusher.PushText(msg); err != nil {
			w.log.Error(err)
		}
		if err = w.rawPusher.PushRaw(push.NewRawTextMessage(msg)); err != nil {
			w.log.Error(err)
		}
		time.Sleep(time.Second)
	}()

	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		w.prs = nil
		w.log.Infof("next checking at %s\n", time.Now().Add(w.config.Interval).Format("2006-01-02 15:04:05"))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			hour := time.Now().Hour()
			if hour >= 0 && hour < 7 {
				// we must sleep in this time
				w.log.Infof("sleeping..")
				continue
			}

			vulns, err := w.collectUpdate(ctx)
			if err != nil {
				w.log.Errorf("failed to get updates, %s", err)
			}
			w.log.Infof("found %d new vulns in this ticking", len(vulns))
			for _, v := range vulns {
				if w.config.NoFilter || v.Creator.IsValuable(v) {
					dbVuln, err := w.db.VulnInformation.Query().Where(vulninformation.Key(v.UniqueKey)).First(ctx)
					if err != nil {
						w.log.Errorf("failed to query %s from db %s", v.UniqueKey, err)
						continue
					}
					if dbVuln.Pushed {
						w.log.Infof("%s has been pushed, skipped", v)
						continue
					}
					if v.CVE != "" && w.config.EnableCVEFilter {
						// 同一个 cve 已经有其它源推送过了
						others, err := w.db.VulnInformation.Query().
							Where(vulninformation.And(vulninformation.Cve(v.CVE), vulninformation.Pushed(true))).All(ctx)
						if err != nil {
							w.log.Errorf("failed to query %s from db %s", v.UniqueKey, err)
							continue
						}
						if len(others) != 0 {
							ids := make([]string, 0, len(others))
							for _, o := range others {
								ids = append(ids, o.Key)
							}
							w.log.Infof("found new cve but other source has already pushed, others: %v", ids)
							continue
						}
					}
					_, err = dbVuln.Update().SetPushed(true).Save(ctx)
					if err != nil {
						w.log.Errorf("failed to save pushed %s status, %s", v.UniqueKey, err)
						continue
					}

					// find cve pr in nuclei repo
					if v.CVE != "" && !w.config.NoNucleiSearch {
						links, err := w.findNucleiPRLink(ctx, v.CVE)
						if err != nil {
							w.log.Warnf("failed to get nuclei link, %s", err)
						}
						w.log.Infof("%s found %d prs from nuclei-templates", v.CVE, len(links))
						if len(links) != 0 {
							v.References = mergeUniqueString(v.References, links)
							_, err = dbVuln.Update().SetReferences(v.References).Save(ctx)
							if err != nil {
								w.log.Warnf("failed to save %s references,  %s", v.UniqueKey, err)
							}
						}
					}
					w.log.Infof("Pushing %s", v)
					err = w.textPusher.PushMarkdown(v.Title, push.RenderVulnInfo(v))
					if err != nil {
						w.log.Errorf("text-pusher send dingding msg error, %s", err)
					}
					err = w.rawPusher.PushRaw(push.NewRawVulnInfoMessage(v))
					if err != nil {
						w.log.Errorf("raw-pusher send dingding msg error, %s", err)
					}
				} else {
					w.log.Infof("skipped %s as not valuable", v)
				}
			}
		}
	}
}

func (w *WatchVulnApp) Close() {
	_ = w.db.Close()
}

func (w *WatchVulnApp) initData(ctx context.Context, grabber grab.Grabber) error {
	pageSize := 100
	source := grabber.ProviderInfo()
	total, err := grabber.GetPageCount(ctx, pageSize)
	if err != nil {
		return nil
	}
	if total == 0 {
		return fmt.Errorf("%s got unexpected zero page", source.Name)
	}
	if total > MaxPageBase {
		total = MaxPageBase
	}
	w.log.Infof("start grab %s, total page: %d", source.Name, total)

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(total)

	for i := 1; i <= total; i++ {
		i := i
		eg.Go(func() error {
			dataChan, err := grabber.ParsePage(ctx, i, pageSize)
			if err != nil {
				return err
			}
			for data := range dataChan {
				if _, err = w.createOrUpdate(ctx, source, data); err != nil {
					return errors.Wrap(err, data.String())
				}
			}
			return nil
		})
	}
	err = eg.Wait()
	if err != nil {
		return err
	}
	return nil
}

func (w *WatchVulnApp) collectUpdate(ctx context.Context) ([]*grab.VulnInfo, error) {
	pageSize := 10
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(len(w.grabbers))

	var mu sync.Mutex
	var newVulns []*grab.VulnInfo

	for _, grabber := range w.grabbers {
		grabber := grabber
		eg.Go(func() error {
			source := grabber.ProviderInfo()
			pageCount, err := grabber.GetPageCount(ctx, pageSize)
			if err != nil {
				return err
			}
			if pageCount > MaxPageBase {
				pageCount = MaxPageBase
			}
			for i := 1; i <= pageCount; i++ {
				dataChan, err := grabber.ParsePage(ctx, i, pageSize)
				if err != nil {
					return err
				}
				hasNewVuln := false

				for data := range dataChan {
					isNewVuln, err := w.createOrUpdate(ctx, source, data)
					if err != nil {
						return err
					}
					if isNewVuln {
						w.log.Infof("found new vuln: %s", data)
						mu.Lock()
						newVulns = append(newVulns, data)
						mu.Unlock()
						hasNewVuln = true
					}
				}

				// 如果一整页漏洞都是旧的，说明没有更新，不必再继续下一页了
				if !hasNewVuln {
					return nil
				}
			}
			return nil
		})
	}
	err := eg.Wait()
	return newVulns, err
}

func (w *WatchVulnApp) createOrUpdate(ctx context.Context, source *grab.Provider, data *grab.VulnInfo) (bool, error) {
	vuln, err := w.db.VulnInformation.Query().
		Where(vulninformation.Key(data.UniqueKey)).
		First(ctx)
	// not exist
	if err != nil {
		data.Reason = append(data.Reason, grab.ReasonNewCreated)
		newVuln, err := w.db.VulnInformation.
			Create().
			SetKey(data.UniqueKey).
			SetTitle(data.Title).
			SetDescription(data.Description).
			SetSeverity(string(data.Severity)).
			SetCve(data.CVE).
			SetDisclosure(data.Disclosure).
			SetSolutions(data.Solutions).
			SetReferences(data.References).
			SetPushed(false).
			SetTags(data.Tags).
			SetFrom(data.From).
			Save(ctx)
		if err != nil {
			return false, err
		}
		w.log.Debugf("vuln %d created from %s %s", newVuln.ID, newVuln.Key, source.Name)
		return true, nil
	}

	// 如果一个漏洞之前是低危，后来改成了严重，这种可能也需要推送, 走一下高价值的判断逻辑
	asNewVuln := false
	if string(data.Severity) != vuln.Severity {
		w.log.Infof("%s from %s change severity from %s to %s", data.Title, data.From, vuln.Severity, data.Severity)
		data.Reason = append(data.Reason, fmt.Sprintf("%s: %s => %s", grab.ReasonSeverityUpdated, vuln.Severity, data.Severity))
		asNewVuln = true
	}
	for _, newTag := range data.Tags {
		found := false
		for _, dbTag := range vuln.Tags {
			if newTag == dbTag {
				found = true
				break
			}
		}
		// tag 有更新
		if !found {
			w.log.Infof("%s from %s add new tag %s", data.Title, data.From, newTag)
			data.Reason = append(data.Reason, fmt.Sprintf("%s: %v => %v", grab.ReasonTagUpdated, vuln.Tags, data.Tags))
			asNewVuln = true
			break
		}
	}

	// update
	newVuln, err := vuln.Update().SetKey(data.UniqueKey).
		SetTitle(data.Title).
		SetDescription(data.Description).
		SetSeverity(string(data.Severity)).
		SetCve(data.CVE).
		SetDisclosure(data.Disclosure).
		SetSolutions(data.Solutions).
		SetReferences(data.References).
		SetTags(data.Tags).
		SetFrom(data.From).
		Save(ctx)
	if err != nil {
		return false, err
	}
	w.log.Debugf("vuln %d updated from %s %s", newVuln.ID, newVuln.Key, source.Name)
	return asNewVuln, nil
}

func (w *WatchVulnApp) findNucleiPRLink(ctx context.Context, cveId string) ([]string, error) {
	if w.prs == nil {
		prs, _, err := w.githubClient.PullRequests.List(ctx, "projectdiscovery", "nuclei-templates", &github.PullRequestListOptions{
			State:       "all",
			ListOptions: github.ListOptions{Page: 1, PerPage: 100},
		})
		if err != nil {
			return nil, err
		}
		w.prs = prs
	}

	var links []string
	re, err := regexp.Compile(fmt.Sprintf(`(?)\b%s\b`, cveId))
	if err != nil {
		return nil, err
	}
	for _, pr := range w.prs {
		if re.MatchString(pr.GetTitle()) || re.MatchString(pr.GetBody()) {
			links = append(links, pr.GetHTMLURL())
		}
	}
	return links, nil
}

func mergeUniqueString(s1 []string, s2 []string) []string {
	m := make(map[string]struct{}, len(s1)+len(s2))
	for _, s := range s1 {
		m[s] = struct{}{}
	}
	for _, s := range s2 {
		m[s] = struct{}{}
	}
	res := make([]string, 0, len(m))
	for k := range m {
		res = append(res, k)
	}
	return res
}