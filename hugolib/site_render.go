// Copyright 2016 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/spf13/hugo/helpers"

	"github.com/spf13/hugo/output"

	bp "github.com/spf13/hugo/bufferpool"
)

// renderPages renders pages each corresponding to a markdown file.
// TODO(bep np doc
func (s *Site) renderPages() error {

	results := make(chan error)
	pages := make(chan *Page)
	errs := make(chan error)

	go errorCollator(results, errs)

	numWorkers := getGoMaxProcs() * 4

	wg := &sync.WaitGroup{}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go pageRenderer(s, pages, results, wg)
	}

	for _, page := range s.Pages {
		pages <- page
	}

	close(pages)

	wg.Wait()

	close(results)

	err := <-errs
	if err != nil {
		return fmt.Errorf("Error(s) rendering pages: %s", err)
	}
	return nil
}

func pageRenderer(s *Site, pages <-chan *Page, results chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()

	for page := range pages {

		for i, outFormat := range page.outputFormats {

			var (
				pageOutput *PageOutput
				err        error
			)

			if i == 0 {
				page.pageOutputInit.Do(func() {
					var po *PageOutput
					po, err = newPageOutput(page, false, outFormat)
					page.mainPageOutput = po
				})
				pageOutput = page.mainPageOutput
			} else {
				pageOutput, err = newPageOutput(page, true, outFormat)
			}

			if err != nil {
				s.Log.ERROR.Printf("Failed to create output page for type %q for page %q: %s", outFormat.Name, page, err)
				continue
			}

			var layouts []string

			if page.selfLayout != "" {
				layouts = []string{page.selfLayout}
			} else {
				layouts, err = s.layouts(pageOutput)
				if err != nil {
					s.Log.ERROR.Printf("Failed to resolve layout output %q for page %q: %s", outFormat.Name, page, err)
					continue
				}
			}

			switch pageOutput.outputFormat.Name {

			case "RSS":
				if err := s.renderRSS(pageOutput); err != nil {
					results <- err
				}
			default:
				targetPath, err := pageOutput.targetPath()
				if err != nil {
					s.Log.ERROR.Printf("Failed to create target path for output %q for page %q: %s", outFormat.Name, page, err)
					continue
				}

				s.Log.DEBUG.Printf("Render %s to %q with layouts %q", pageOutput.Kind, targetPath, layouts)

				if err := s.renderAndWritePage("page "+pageOutput.FullFilePath(), targetPath, pageOutput, layouts...); err != nil {
					results <- err
				}

				if pageOutput.IsNode() {
					if err := s.renderPaginator(pageOutput); err != nil {
						results <- err
					}
				}
			}

		}
	}
}

// renderPaginator must be run after the owning Page has been rendered.
func (s *Site) renderPaginator(p *PageOutput) error {
	if p.paginator != nil {
		s.Log.DEBUG.Printf("Render paginator for page %q", p.Path())
		paginatePath := s.Cfg.GetString("paginatePath")

		// write alias for page 1
		addend := fmt.Sprintf("/%s/%d", paginatePath, 1)
		target, err := p.createTargetPath(p.outputFormat, addend)
		if err != nil {
			return err
		}

		// TODO(bep) do better
		link := newOutputFormat(p.Page, p.outputFormat).Permalink()
		if err := s.writeDestAlias(target, link, nil); err != nil {
			return err
		}

		pagers := p.paginator.Pagers()

		for i, pager := range pagers {
			if i == 0 {
				// already created
				continue
			}

			pagerNode := p.copy()

			pagerNode.paginator = pager
			if pager.TotalPages() > 0 {
				first, _ := pager.page(0)
				pagerNode.Date = first.Date
				pagerNode.Lastmod = first.Lastmod
			}

			pageNumber := i + 1
			addend := fmt.Sprintf("/%s/%d", paginatePath, pageNumber)
			targetPath, _ := p.targetPath(addend)
			layouts, err := p.layouts()

			if err != nil {
				return err
			}

			if err := s.renderAndWritePage(
				pagerNode.Title,
				targetPath, pagerNode, layouts...); err != nil {
				return err
			}

		}
	}
	return nil
}

func (s *Site) renderRSS(p *PageOutput) error {

	if !s.isEnabled(kindRSS) {
		return nil
	}

	if s.Cfg.GetBool("disableRSS") {
		return nil
	}

	p.Kind = kindRSS

	// TODO(bep) we zero the date here to get the number of diffs down in
	// testing. But this should be set back later; the RSS feed should
	// inherit the publish date from the node it represents.
	if p.Kind == KindTaxonomy {
		var zeroDate time.Time
		p.Date = zeroDate
	}

	limit := s.Cfg.GetInt("rssLimit")
	if limit >= 0 && len(p.Pages) > limit {
		p.Pages = p.Pages[:limit]
		p.Data["Pages"] = p.Pages
	}

	layouts, err := s.layoutHandler.For(
		p.layoutDescriptor,
		"",
		p.outputFormat)
	if err != nil {
		return err
	}

	targetPath, err := p.targetPath()
	if err != nil {
		return err
	}

	return s.renderAndWriteXML(p.Title,
		targetPath, p, layouts...)
}

func (s *Site) render404() error {
	if !s.isEnabled(kind404) {
		return nil
	}

	if s.Cfg.GetBool("disable404") {
		return nil
	}

	p := s.newNodePage(kind404)

	p.Title = "404 Page not found"
	p.Data["Pages"] = s.Pages
	p.Pages = s.Pages
	p.URLPath.URL = "404.html"

	if err := p.initTargetPathDescriptor(); err != nil {
		return err
	}

	nfLayouts := []string{"404.html"}

	pageOutput, err := newPageOutput(p, false, output.HTMLFormat)
	if err != nil {
		return err
	}

	return s.renderAndWritePage("404 page", "404.html", pageOutput, s.appendThemeTemplates(nfLayouts)...)

}

func (s *Site) renderSitemap() error {
	if !s.isEnabled(kindSitemap) {
		return nil
	}

	if s.Cfg.GetBool("disableSitemap") {
		return nil
	}

	sitemapDefault := parseSitemap(s.Cfg.GetStringMap("sitemap"))

	n := s.newNodePage(kindSitemap)

	// Include all pages (regular, home page, taxonomies etc.)
	pages := s.Pages

	page := s.newNodePage(kindSitemap)
	page.URLPath.URL = ""
	if err := page.initTargetPathDescriptor(); err != nil {
		return err
	}
	page.Sitemap.ChangeFreq = sitemapDefault.ChangeFreq
	page.Sitemap.Priority = sitemapDefault.Priority
	page.Sitemap.Filename = sitemapDefault.Filename

	n.Data["Pages"] = pages
	n.Pages = pages

	// TODO(bep) output
	if err := page.initTargetPathDescriptor(); err != nil {
		return err
	}

	// TODO(bep) this should be done somewhere else
	for _, page := range pages {
		if page.Sitemap.ChangeFreq == "" {
			page.Sitemap.ChangeFreq = sitemapDefault.ChangeFreq
		}

		if page.Sitemap.Priority == -1 {
			page.Sitemap.Priority = sitemapDefault.Priority
		}

		if page.Sitemap.Filename == "" {
			page.Sitemap.Filename = sitemapDefault.Filename
		}
	}

	smLayouts := []string{"sitemap.xml", "_default/sitemap.xml", "_internal/_default/sitemap.xml"}
	addLanguagePrefix := n.Site.IsMultiLingual()

	return s.renderAndWriteXML("sitemap",
		n.addLangPathPrefixIfFlagSet(page.Sitemap.Filename, addLanguagePrefix), n, s.appendThemeTemplates(smLayouts)...)
}

func (s *Site) renderRobotsTXT() error {
	if !s.isEnabled(kindRobotsTXT) {
		return nil
	}

	if !s.Cfg.GetBool("enableRobotsTXT") {
		return nil
	}

	n := s.newNodePage(kindRobotsTXT)
	if err := n.initTargetPathDescriptor(); err != nil {
		return err
	}
	n.Data["Pages"] = s.Pages
	n.Pages = s.Pages

	rLayouts := []string{"robots.txt", "_default/robots.txt", "_internal/_default/robots.txt"}
	outBuffer := bp.GetBuffer()
	defer bp.PutBuffer(outBuffer)
	if err := s.renderForLayouts("robots", n, outBuffer, s.appendThemeTemplates(rLayouts)...); err != nil {
		helpers.DistinctWarnLog.Println(err)
		return nil
	}

	return s.publish("robots.txt", outBuffer)
}

// renderAliases renders shell pages that simply have a redirect in the header.
func (s *Site) renderAliases() error {
	for _, p := range s.Pages {
		if len(p.Aliases) == 0 {
			continue
		}

		for _, f := range p.outputFormats {
			if !f.IsHTML {
				continue
			}

			o := newOutputFormat(p, f)
			plink := o.Permalink()

			for _, a := range p.Aliases {
				if f.Path != "" {
					// Make sure AMP and similar doesn't clash with regular aliases.
					a = path.Join(a, f.Path)
				}

				if err := s.writeDestAlias(a, plink, p); err != nil {
					return err
				}
			}
		}
	}

	if s.owner.multilingual.enabled() {
		mainLang := s.owner.multilingual.DefaultLang
		if s.Info.defaultContentLanguageInSubdir {
			mainLangURL := s.PathSpec.AbsURL(mainLang.Lang, false)
			s.Log.DEBUG.Printf("Write redirect to main language %s: %s", mainLang, mainLangURL)
			if err := s.publishDestAlias(true, "/", mainLangURL, nil); err != nil {
				return err
			}
		} else {
			mainLangURL := s.PathSpec.AbsURL("", false)
			s.Log.DEBUG.Printf("Write redirect to main language %s: %s", mainLang, mainLangURL)
			if err := s.publishDestAlias(true, mainLang.Lang, mainLangURL, nil); err != nil {
				return err
			}
		}
	}

	return nil
}
