// Copyright © 2013 Steve Francia <spf@spf13.com>.
//
// Licensed under the Simple Public License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://opensource.org/licenses/Simple-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
    "bytes"
    "errors"
    "fmt"
    "github.com/BurntSushi/toml"
    "github.com/spf13/hugo/helpers"
    "github.com/spf13/hugo/parser"
    "github.com/spf13/hugo/template/bundle"
    "github.com/theplant/blackfriday"
    "html/template"
    "io"
    "launchpad.net/goyaml"
    json "launchpad.net/rjson"
    "net/url"
    "path"
    "strings"
    "time"
)

type Page struct {
    Status      string
    Images      []string
    rawContent  []byte
    Content     template.HTML
    Summary     template.HTML
    Truncated   bool
    plain       string // TODO should be []byte
    Params      map[string]interface{}
    contentType string
    Draft       bool
    Aliases     []string
    Tmpl        bundle.Template
    Markup      string
    renderable  bool
    layout      string
    linkTitle   string
    PageMeta
    File
    Position
    Node
}

type File struct {
    FileName, Extension, Dir string
}

type PageMeta struct {
    WordCount      int
    FuzzyWordCount int
    ReadingTime    int
    Weight         int
}

type Position struct {
    Prev *Page
    Next *Page
}

type Pages []*Page

func (p *Page) Plain() string {
    if len(p.plain) == 0 {
        p.plain = StripHTML(StripShortcodes(string(p.rawContent)))
    }
    return p.plain
}

func (p *Page) setSummary() {
    if bytes.Contains(p.rawContent, summaryDivider) {
        // If user defines split:
        // Split then render
        p.Truncated = true // by definition
        header := string(bytes.Split(p.rawContent, summaryDivider)[0])
        p.Summary = bytesToHTML(p.renderBytes([]byte(ShortcodesHandle(header, p, p.Tmpl))))
    } else {
        // If hugo defines split:
        // render, strip html, then split
        plain := strings.TrimSpace(p.Plain())
        p.Summary = bytesToHTML([]byte(TruncateWordsToWholeSentence(plain, summaryLength)))
        p.Truncated = len(p.Summary) != len(plain)
    }
}

func bytesToHTML(b []byte) template.HTML {
    return template.HTML(string(b))
}

func (p *Page) renderBytes(content []byte) []byte {
    return renderBytes(content, p.guessMarkupType())
}

func (p *Page) renderString(content string) []byte {
    return renderBytes([]byte(content), p.guessMarkupType())
}

func renderBytes(content []byte, pagefmt string) []byte {
    switch pagefmt {
    default:
        return blackfriday.MarkdownCommon(content)
    case "markdown":
        return blackfriday.MarkdownCommon(content)
    case "rst":
        return []byte(getRstContent(content))
    }
}

// TODO abstract further to support loading from more
// than just files on disk. Should load reader (file, []byte)
func newPage(filename string) *Page {
    page := Page{contentType: "",
        File:   File{FileName: filename, Extension: "html"},
        Node:   Node{Keywords: make([]string, 10, 30)},
        Params: make(map[string]interface{})}
    page.Date, _ = time.Parse("20060102", "20080101")
    page.guessSection()
    return &page
}

func StripHTML(s string) string {
    output := ""

    // Shortcut strings with no tags in them
    if !strings.ContainsAny(s, "<>") {
        output = s
    } else {
        s = strings.Replace(s, "\n", " ", -1)
        s = strings.Replace(s, "</p>", " \n", -1)
        s = strings.Replace(s, "<br>", " \n", -1)
        s = strings.Replace(s, "</br>", " \n", -1)

        // Walk through the string removing all tags
        b := new(bytes.Buffer)
        inTag := false
        for _, r := range s {
            switch r {
            case '<':
                inTag = true
            case '>':
                inTag = false
            default:
                if !inTag {
                    b.WriteRune(r)
                }
            }
        }
        output = b.String()
    }
    return output
}

func (p *Page) IsRenderable() bool {
    return p.renderable
}

func (p *Page) guessSection() {
    if p.Section == "" {
        x := strings.Split(p.FileName, "/")
        x = x[:len(x)-1]
        if len(x) == 0 {
            return
        }
        if x[0] == "content" {
            x = x[1:]
        }
        p.Section = path.Join(x...)
    }
}

func (page *Page) Type() string {
    if page.contentType != "" {
        return page.contentType
    }
    page.guessSection()
    if x := page.Section; x != "" {
        return x
    }

    return "page"
}

func (page *Page) Layout(l ...string) []string {
    if page.layout != "" {
        return layouts(page.Type(), page.layout)
    }

    layout := ""
    if len(l) == 0 {
        layout = "single"
    } else {
        layout = l[0]
    }

    return layouts(page.Type(), layout)
}

func layouts(types string, layout string) (layouts []string) {
    t := strings.Split(types, "/")
    for i := range t {
        search := t[:len(t)-i]
        layouts = append(layouts, fmt.Sprintf("%s/%s.html", strings.ToLower(path.Join(search...)), layout))
    }
    layouts = append(layouts, fmt.Sprintf("%s.html", layout))
    return
}

func ReadFrom(buf io.Reader, name string) (page *Page, err error) {
    if len(name) == 0 {
        return nil, errors.New("Zero length page name")
    }

    // Create new page
    p := newPage(name)

    // Parse for metadata & body
    if err = p.parse(buf); err != nil {
        return
    }

    //analyze for raw stats
    p.analyzePage()

    return p, nil
}

func (p *Page) analyzePage() {
    p.WordCount = TotalWords(p.Plain())
    p.FuzzyWordCount = int((p.WordCount+100)/100) * 100
    p.ReadingTime = int((p.WordCount + 212) / 213)
}

func (p *Page) permalink() (*url.URL, error) {
    baseUrl := string(p.Site.BaseUrl)
    dir := strings.TrimSpace(p.Dir)
    pSlug := strings.TrimSpace(p.Slug)
    pUrl := strings.TrimSpace(p.Url)
    var permalink string
    var err error

    if override, ok := p.Site.Permalinks[p.Section]; ok {
        permalink, err = override.Expand(p)
        if err != nil {
            return nil, err
        }
        //fmt.Printf("have an override for %q in section %s → %s\n", p.Title, p.Section, permalink)
    } else {

        if len(pSlug) > 0 {
            if p.Site.Config != nil && p.Site.Config.UglyUrls {
                permalink = path.Join(dir, p.Slug, p.Extension)
            } else {
                permalink = path.Join(dir, p.Slug) + "/"
            }
        } else if len(pUrl) > 2 {
            permalink = pUrl
        } else {
            _, t := path.Split(p.FileName)
            if p.Site.Config != nil && p.Site.Config.UglyUrls {
                x := replaceExtension(strings.TrimSpace(t), p.Extension)
                permalink = path.Join(dir, x)
            } else {
                file, _ := fileExt(strings.TrimSpace(t))
                permalink = path.Join(dir, file)
            }
        }

    }

    base, err := url.Parse(baseUrl)
    if err != nil {
        return nil, err
    }

    path, err := url.Parse(permalink)
    if err != nil {
        return nil, err
    }

    return MakePermalink(base, path), nil
}

func (p *Page) LinkTitle() string {
    if len(p.linkTitle) > 0 {
        return p.linkTitle
    } else {
        return p.Title
    }
}

func (p *Page) Permalink() (string, error) {
    link, err := p.permalink()
    if err != nil {
        return "", err
    }
    return link.String(), nil
}

func (p *Page) RelPermalink() (string, error) {
    link, err := p.permalink()
    if err != nil {
        return "", err
    }

    link.Scheme = ""
    link.Host = ""
    link.User = nil
    link.Opaque = ""
    return link.String(), nil
}

func (page *Page) handleTomlMetaData(datum []byte) (interface{}, error) {
    m := map[string]interface{}{}
    datum = removeTomlIdentifier(datum)
    if _, err := toml.Decode(string(datum), &m); err != nil {
        return m, fmt.Errorf("Invalid TOML in %s \nError parsing page meta data: %s", page.FileName, err)
    }
    return m, nil
}

func removeTomlIdentifier(datum []byte) []byte {
    return bytes.Replace(datum, []byte("+++"), []byte(""), -1)
}

func (page *Page) handleYamlMetaData(datum []byte) (interface{}, error) {
    m := map[string]interface{}{}
    if err := goyaml.Unmarshal(datum, &m); err != nil {
        return m, fmt.Errorf("Invalid YAML in %s \nError parsing page meta data: %s", page.FileName, err)
    }
    return m, nil
}

func (page *Page) handleJsonMetaData(datum []byte) (interface{}, error) {
    var f interface{}
    if err := json.Unmarshal(datum, &f); err != nil {
        return f, fmt.Errorf("Invalid JSON in %v \nError parsing page meta data: %s", page.FileName, err)
    }
    return f, nil
}

func (page *Page) update(f interface{}) error {
    m := f.(map[string]interface{})

    for k, v := range m {
        loki := strings.ToLower(k)
        switch loki {
        case "title":
            page.Title = interfaceToString(v)
        case "linktitle":
            page.linkTitle = interfaceToString(v)
        case "description":
            page.Description = interfaceToString(v)
        case "slug":
            page.Slug = helpers.Urlize(interfaceToString(v))
        case "url":
            if url := interfaceToString(v); strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
                return fmt.Errorf("Only relative urls are supported, %v provided", url)
            }
            page.Url = helpers.Urlize(interfaceToString(v))
        case "type":
            page.contentType = interfaceToString(v)
        case "keywords":
            page.Keywords = interfaceArrayToStringArray(v)
        case "date", "pubdate":
            page.Date = interfaceToTime(v)
        case "draft":
            page.Draft = interfaceToBool(v)
        case "layout":
            page.layout = interfaceToString(v)
        case "markup":
            page.Markup = interfaceToString(v)
        case "weight":
            page.Weight = interfaceToInt(v)
        case "aliases":
            page.Aliases = interfaceArrayToStringArray(v)
            for _, alias := range page.Aliases {
                if strings.HasPrefix(alias, "http://") || strings.HasPrefix(alias, "https://") {
                    return fmt.Errorf("Only relative aliases are supported, %v provided", alias)
                }
            }
        case "status":
            page.Status = interfaceToString(v)
        default:
            // If not one of the explicit values, store in Params
            switch vv := v.(type) {
            case string:
                page.Params[loki] = vv
            case int64, int32, int16, int8, int:
                page.Params[loki] = vv
            case float64, float32:
                page.Params[loki] = vv
            case time.Time:
                page.Params[loki] = vv
            default: // handle array of strings as well
                switch vvv := vv.(type) {
                case []interface{}:
                    var a = make([]string, len(vvv))
                    for i, u := range vvv {
                        a[i] = interfaceToString(u)
                    }
                    page.Params[loki] = a
                }
            }
        }
    }
    return nil

}

func (page *Page) GetParam(key string) interface{} {
    v := page.Params[strings.ToLower(key)]

    if v == nil {
        return nil
    }

    switch v.(type) {
    case string:
        return interfaceToString(v)
    case int64, int32, int16, int8, int:
        return interfaceToInt(v)
    case float64, float32:
        return interfaceToFloat64(v)
    case time.Time:
        return interfaceToTime(v)
    case []string:
        return v
    }
    return nil
}

type frontmatterType struct {
    markstart, markend []byte
    parse              func([]byte) (interface{}, error)
    includeMark        bool
}

const YAML_DELIM = "---"
const TOML_DELIM = "+++"

func (page *Page) detectFrontMatter(mark rune) (f *frontmatterType) {
    switch mark {
    case '-':
        return &frontmatterType{[]byte(YAML_DELIM), []byte(YAML_DELIM), page.handleYamlMetaData, false}
    case '+':
        return &frontmatterType{[]byte(TOML_DELIM), []byte(TOML_DELIM), page.handleTomlMetaData, false}
    case '{':
        return &frontmatterType{[]byte{'{'}, []byte{'}'}, page.handleJsonMetaData, true}
    default:
        return nil
    }
}

func (p *Page) Render(layout ...string) template.HTML {
    curLayout := ""

    if len(layout) > 0 {
        curLayout = layout[0]
    }

    return bytesToHTML(p.ExecuteTemplate(curLayout).Bytes())
}

func (p *Page) ExecuteTemplate(layout string) *bytes.Buffer {
    l := p.Layout(layout)
    buffer := new(bytes.Buffer)
    for _, layout := range l {
        if p.Tmpl.Lookup(layout) != nil {
            p.Tmpl.ExecuteTemplate(buffer, layout, p)
            break
        }
    }
    return buffer
}

func (page *Page) guessMarkupType() string {
    // First try the explicitly set markup from the frontmatter
    if page.Markup != "" {
        format := guessType(page.Markup)
        if format != "unknown" {
            return format
        }
    }

    // Then try to guess from the extension
    ext := strings.ToLower(path.Ext(page.FileName))
    if strings.HasPrefix(ext, ".") {
        return guessType(ext[1:])
    }

    return "unknown"
}

func guessType(in string) string {
    switch strings.ToLower(in) {
    case "md", "markdown", "mdown":
        return "markdown"
    case "rst":
        return "rst"
    case "html", "htm":
        return "html"
    }
    return "unknown"
}

func (page *Page) parse(reader io.Reader) error {
    p, err := parser.ReadFrom(reader)
    if err != nil {
        return err
    }

    page.renderable = p.IsRenderable()

    front := p.FrontMatter()

    if len(front) != 0 {
        fm := page.detectFrontMatter(rune(front[0]))
        meta, err := fm.parse(front)
        if err != nil {
            return err
        }

        if err = page.update(meta); err != nil {
            return err
        }

    }
    page.rawContent = p.Content()
    page.setSummary()

    return nil
}

func (p *Page) ProcessShortcodes(t bundle.Template) {
    p.rawContent = []byte(ShortcodesHandle(string(p.rawContent), p, t))
    p.Summary = template.HTML(ShortcodesHandle(string(p.Summary), p, t))
}

func (page *Page) Convert() error {
    markupType := page.guessMarkupType()
    switch markupType {
    case "markdown", "rst":
        page.Content = bytesToHTML(page.renderString(string(RemoveSummaryDivider(page.rawContent))))
    case "html":
        page.Content = bytesToHTML(page.rawContent)
    default:
        return errors.New("Error converting unsupported file type " + markupType)
    }
    return nil
}

// Lazily generate the TOC
func (page *Page) TableOfContents() template.HTML {
    return tableOfContentsFromBytes([]byte(page.Content))
}

func tableOfContentsFromBytes(content []byte) template.HTML {
    htmlFlags := 0
    htmlFlags |= blackfriday.HTML_SKIP_SCRIPT
    htmlFlags |= blackfriday.HTML_TOC
    htmlFlags |= blackfriday.HTML_OMIT_CONTENTS
    renderer := blackfriday.HtmlRenderer(htmlFlags, "", "")

    return template.HTML(string(blackfriday.Markdown(RemoveSummaryDivider(content), renderer, 0)))
}

func ReaderToBytes(lines io.Reader) []byte {
    b := new(bytes.Buffer)
    b.ReadFrom(lines)
    return b.Bytes()
}

func (p *Page) TargetPath() (outfile string) {

    // Always use Url if it's specified
    if len(strings.TrimSpace(p.Url)) > 2 {
        outfile = strings.TrimSpace(p.Url)

        if strings.HasSuffix(outfile, "/") {
            outfile = outfile + "index.html"
        }
        return
    }

    // If there's a Permalink specification, we use that
    if override, ok := p.Site.Permalinks[p.Section]; ok {
        var err error
        outfile, err = override.Expand(p)
        if err == nil {
            if strings.HasSuffix(outfile, "/") {
                outfile += "index.html"
            }
            return
        }
    }

    if len(strings.TrimSpace(p.Slug)) > 0 {
        outfile = strings.TrimSpace(p.Slug) + "." + p.Extension
    } else {
        // Fall back to filename
        _, t := path.Split(p.FileName)
        outfile = replaceExtension(strings.TrimSpace(t), p.Extension)
    }

    return path.Join(p.Dir, strings.TrimSpace(outfile))
}
