package ai_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// NewsSource represents a single news source with its details
type NewsSource struct {
	Name     string
	URL      string
	Category string
	APIPath  string
}

// NewsArticle represents a single news article
type NewsArticle struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Source      string    `json:"source"`
	PublishedAt time.Time `json:"publishedAt"`
	Content     string    `json:"content,omitempty"`
}

var NewsSearchTool = utils.AITool{
	Name:        "News Search",
	Description: "Search for news articles across multiple sources by domain or keyword",
	ToolID:      "search_news",
	ToolRequest: utils.ToolsRequest{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "search_news",
			Description: "Search for news articles across multiple sources by domain or keyword",
			Parameters: &utils.ParameterToolsRequest{
				Type: "object",
				Properties: map[string]utils.PropertyParameterToolsRequest{
					"keyword": {
						Type:        "string",
						Description: "Optional: Specific keyword or topic to search for. Leave empty to get top headlines",
					},
					/*"country": {
							Type:        "string",
							Description: "Optional: Specific country to search for news",
					},*/
					"language": {
						Type:        "string",
						Description: "REQUIRED: language code to search news with (e.g. 'en', 'fr', 'es')",
					},
				},
				Required: []string{"language"},
			},
		},
	},
	LoadingString: "Searching news...",
	IconURL:       "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAACXBIWXMAAAsTAAALEwEAmpwYAAAGG2lUWHRYTUw6Y29tLmFkb2JlLnhtcAAAAAAAPD94cGFja2V0IGJlZ2luPSLvu78iIGlkPSJXNU0wTXBDZWhpSHpyZVN6TlRjemtjOWQiPz4gPHg6eG1wbWV0YSB4bWxuczp4PSJhZG9iZTpuczptZXRhLyIgeDp4bXB0az0iQWRvYmUgWE1QIENvcmUgOS4xLWMwMDIgNzkuYTZhNjM5NiwgMjAyNC8wMy8xMi0wNzo0ODoyMyAgICAgICAgIj4gPHJkZjpSREYgeG1sbnM6cmRmPSJodHRwOi8vd3d3LnczLm9yZy8xOTk5LzAyLzIyLXJkZi1zeW50YXgtbnMjIj4gPHJkZjpEZXNjcmlwdGlvbiByZGY6YWJvdXQ9IiIgeG1sbnM6eG1wPSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvIiB4bWxuczpkYz0iaHR0cDovL3B1cmwub3JnL2RjL2VsZW1lbnRzLzEuMS8iIHhtbG5zOnBob3Rvc2hvcD0iaHR0cDovL25zLmFkb2JlLmNvbS9waG90b3Nob3AvMS4wLyIgeG1sbnM6eG1wTU09Imh0dHA6Ly9ucy5hZG9iZS5jb20veGFwLzEuMC9tbS8iIHhtbG5zOnN0RXZ0PSJodHRwOi8vbnMuYWRvYmUuY29tL3hhcC8xLjAvc1R5cGUvUmVzb3VyY2VFdmVudCMiIHhtcDpDcmVhdG9yVG9vbD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgeG1wOkNyZWF0ZURhdGU9IjIwMjUtMDMtMTRUMTE6MDM6MTBaIiB4bXA6TW9kaWZ5RGF0ZT0iMjAyNS0wMy0xNFQxNjo0NDoxOVoiIHhtcDpNZXRhZGF0YURhdGU9IjIwMjUtMDMtMTRUMTY6NDQ6MTlaIiBkYzpmb3JtYXQ9ImltYWdlL3BuZyIgcGhvdG9zaG9wOkNvbG9yTW9kZT0iMyIgeG1wTU06SW5zdGFuY2VJRD0ieG1wLmlpZDowYzAzMmI0Yi03NGZjLWY1NDUtYTNkNi0zYmFiMTg4MmFjOTQiIHhtcE1NOkRvY3VtZW50SUQ9ImFkb2JlOmRvY2lkOnBob3Rvc2hvcDo0ZWQwNzEzOC1kZTIzLTA2NDUtOGRmNi01NmEwNDY1ZDYzYjUiIHhtcE1NOk9yaWdpbmFsRG9jdW1lbnRJRD0ieG1wLmRpZDpmMjgzYTIwMi04MjRkLWExNGEtYWYwZS04MzAwZmQ4OGQ4NDciPiA8eG1wTU06SGlzdG9yeT4gPHJkZjpTZXE+IDxyZGY6bGkgc3RFdnQ6YWN0aW9uPSJjcmVhdGVkIiBzdEV2dDppbnN0YW5jZUlEPSJ4bXAuaWlkOmYyODNhMjAyLTgyNGQtYTE0YS1hZjBlLTgzMDBmZDg4ZDg0NyIgc3RFdnQ6d2hlbj0iMjAyNS0wMy0xNFQxMTowMzoxMFoiIHN0RXZ0OnNvZnR3YXJlQWdlbnQ9IkFkb2JlIFBob3Rvc2hvcCAyNS4xMiAoV2luZG93cykiLz4gPHJkZjpsaSBzdEV2dDphY3Rpb249ImNvbnZlcnRlZCIgc3RFdnQ6cGFyYW1ldGVycz0iZnJvbSBhcHBsaWNhdGlvbi92bmQuYWRvYmUucGhvdG9zaG9wIHRvIGltYWdlL3BuZyIvPiA8cmRmOmxpIHN0RXZ0OmFjdGlvbj0ic2F2ZWQiIHN0RXZ0Omluc3RhbmNlSUQ9InhtcC5paWQ6MGMwMzJiNGItNzRmYy1mNTQ1LWEzZDYtM2JhYjE4ODJhYzk0IiBzdEV2dDp3aGVuPSIyMDI1LTAzLTE0VDE2OjQ0OjE5WiIgc3RFdnQ6c29mdHdhcmVBZ2VudD0iQWRvYmUgUGhvdG9zaG9wIDI1LjEyIChXaW5kb3dzKSIgc3RFdnQ6Y2hhbmdlZD0iLyIvPiA8L3JkZjpTZXE+IDwveG1wTU06SGlzdG9yeT4gPC9yZGY6RGVzY3JpcHRpb24+IDwvcmRmOlJERj4gPC94OnhtcG1ldGE+IDw/eHBhY2tldCBlbmQ9InIiPz6ToeHzAAAFH0lEQVR42u1ba0hbVxz/JTevqzF6WFlpQ/qgm1LrCKNG6vrBTrqKpYPtUxG6KnT7MhjbPgxK7YN2g1G6TYorisjYhzFwH8ZYR2GDDhlO0gejMJQaSq2oHWvFYxPNw+nNPqQmua8k9+RqEnP/EMw9N+fx/53z+z/OOZri8TjKWcwoczEAMAAwADAAMAAwADAAKF+xaK1AT516H0eONAIAVlflP4jHAZMp9Vdarvas1EYu79L7A+T1zM/neHx8As3NA6S9nTIDQPv727A48yF+/Ka2BCf7H2zZQtHePpBeaNKSC9DjbY8BbCvlJU+GfjUx2QAaCNgBWEuc8svMNsC1Z48jCFSKCr0twPKyOhelvJTah/TVp2YflLidax8OB3DPDyC6VjPGDEAkEhEArCQLaptAzpwp+imn168D332dvxsUBEFcEAyWxqKfm9MnDnA6nXJ3VE6BkNlsDgFWoeQ0zDJRijaA9vbuw5071eD5RCAx/xh4rbUC+M+qaLSk9WdmgFgM4Dj1AAUAVlbgcLvB87zsVTgcRmx2FrBasysoCADPg2zfLn+fYZyKANBr1z7DyC/dMps5eiMr2LFYDOGuLkCYz3mCogCiH18EOXAgNYaREaD3U+0Gz70PritXwHFcHhT44+du1qUV9vs1KZ+Unq/EzwzKAwBmxxCcnNREASUbEM6pM5UlzSScZCG++Q4z5W3btAWqSjYgLcNxAO99kEJx8IssinCMo7aJw9UTJ0D37gWePEm0mYnHazbAakVVSwssFkveAKTkpQaQw4dTHEsHQE83qNAW2b+/CNxgKKSNAsUYG2j1AuvZmRYJBoNYffo01aY0d0jv8zkFyI4d+sQBzCIIutSjo6PA1Yva3eDWOrh6evJ0g/mI0g5RTkmqhGoMygMA/p2Qu8Esq1JXAEw+H1vF4yfFz291MY/B4Xbr6AU0Wu6amhpEvv0J0Zs35Tm9dCbi8cRn504Qr1fsATo6QOvrgampRCieaY9wjUJ2O0hbm842gMGq8zwP/tix/LeuvF5AAsx6GGbzejVc0tlgPkrS6WlgaUkeFaq5Mb2Mr8MBsmtXYeMAeu4cELhdsMmmVR44+/pgtea+d2tmXj6Sd5TSgiqfiFynsTg1lXc2yEQBl8sFVHkKTHgr7NJNkY2iAMdxIIODoHfvAtFown0puT7psZnaFnemnSRpfUEAbDaQpibN42Z3gyrIksZGlJKwU2CT7AqzU0AFHHr/fooCubahBqYaXZQiQYsFpKFhAwGQDEYQBDy7cKGwbrDSjaqBAU27QrolQ88WFgrvBpdmEXr0SEcboEFc1dUA2V1wRld4PDpSQIOh4zgOpL8f1O8HIhGxG1S7xaF2oqx2cqx0O2TNBthsIAcP6mwDGHKB9AOOopAsYbFZbwCKTmIxcaiYAwD25LeKCvWGWQ9BNlp++yH9aT4XACLJbw//SgNScrmirq7odaeXL4sLPK/cym4DyO7fQSffTjbS2Qm0tgI3vhf/LhAAHRrKfFVOzbDmen1OrZ1MeYLZnNiPEM98Qjo6ZFdaZLfE6PBwPfo+H8Nmk1df/4icPn01KwXIoUPj8L1xcnNpX/mJkvKKKyC5Ei5dasbYn90AfABeAMCViLYrAIIAQkDlMDrf/ZIcPfq3qqPLdFGSnj/fionbzUB8K16sdSavxCklLGpl+ewFZrteJ23bYgHmHiwCmMfLvgU4HMPk7Nl7Gbsw/muszMUAwADAAMAAwADAAMAAoHzlfxAr6M/gJPGiAAAAAElFTkSuQmCC",
	Exec: func(_ context.Context, input string, _ utils.Conversation) utils.MessageContent {
		// Parse the parameters from JSON
		var params struct {
			Country  string `json:"country"`
			Keyword  string `json:"keyword"`
			Language string `json:"language"`
		}

		err := json.Unmarshal([]byte(input), &params)
		if err != nil {
			return utils.NewTextContent(fmt.Sprintf("Error parsing parameters: %s", err.Error()))
		}

		// Search for news
		articles := searchNewsFromSources(params.Keyword, params.Country, params.Language)

		// Format the results
		return utils.NewTextContent(formatNewsResults(articles, params.Keyword))
	},
}

// searchNewsFromSources searches for news across multiple sources
func searchNewsFromSources(keyword string, country string, language string) []NewsArticle {
	var articles []NewsArticle

	// In a real implementation, you would use a news API like NewsAPI.org
	// Here's a simplified example using the NewsAPI endpoint
	apiKey := os.Getenv("NEWS_API_KEY")

	var apiURL string

	if keyword != "" {
		// URL sanity
		keyword = strings.Replace(keyword, " ", "%20", -1)

		// Build the API URL
		apiURL = fmt.Sprintf("https://newsapi.org/v2/everything?pageSize=20&language=%s&q=%s&apiKey=%s",
			language, keyword, apiKey)
	} else {
		// Build the API URL
		apiURL = fmt.Sprintf("https://newsapi.org/v2/top-headlines?pageSize=20&language=%s&apiKey=%s", language, apiKey)
	}

	utils.Debug("API URL: %s", apiURL)

	if country != "" {
		apiURL = fmt.Sprintf("%s&country=%s", apiURL, country)
	}

	// Make the API request
	response, err := utils.HTTPClient.Get(apiURL)
	if err != nil {
		utils.Error("Error making API request", err)
		return []NewsArticle{}
	}
	defer response.Body.Close()

	// Parse the response
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		utils.Error("Error making API request", err)
		return []NewsArticle{}
	}

	var apiResponse struct {
		Status       string `json:"status"`
		TotalResults int    `json:"totalResults"`
		Articles     []struct {
			Source struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"source"`
			Author      string    `json:"author"`
			Title       string    `json:"title"`
			Description string    `json:"description"`
			URL         string    `json:"url"`
			PublishedAt time.Time `json:"publishedAt"`
			Content     string    `json:"content"`
		} `json:"articles"`
	}

	err = json.Unmarshal(body, &apiResponse)
	if err != nil {
		utils.Error("Error parsing API response", err)
		return []NewsArticle{}
	}

	// Convert API articles to our format
	for _, article := range apiResponse.Articles {
		articles = append(articles, NewsArticle{
			Title:       article.Title,
			Description: article.Description,
			URL:         article.URL,
			Source:      article.Source.Name,
			PublishedAt: article.PublishedAt,
			Content:     article.Content,
		})
	}

	// Alternative implementation without external API:
	// You could also implement web scraping for each source
	// using goquery similar to your WebTool implementation

	return articles
}

// formatNewsResults formats the news articles into a readable string
func formatNewsResults(articles []NewsArticle, keyword string) string {
	var result strings.Builder

	// Create the header
	if keyword != "" {
		result.WriteString(fmt.Sprintf("# News Results for '%s'\n\n", keyword))
	} else {
		result.WriteString("# Latest News\n\n")
	}

	// Check if we found any articles
	if len(articles) == 0 {
		result.WriteString("No news articles found. Try a different theme or keyword.\n")
		return result.String()
	}

	// Group articles by source
	sourceMap := make(map[string][]NewsArticle)
	for _, article := range articles {
		sourceMap[article.Source] = append(sourceMap[article.Source], article)
	}

	// Write articles grouped by source
	for source, sourceArticles := range sourceMap {
		result.WriteString(fmt.Sprintf("## %s\n\n", source))

		for _, article := range sourceArticles {
			// Format publication date
			dateStr := article.PublishedAt.Format("Jan 2, 2006")

			result.WriteString(fmt.Sprintf("### [%s](%s)\n", article.Title, article.URL))
			result.WriteString(fmt.Sprintf("*Published on %s*\n\n", dateStr))
			result.WriteString(fmt.Sprintf("%s\n\n", article.Description))
			result.WriteString(fmt.Sprintf("[Read more](%s)\n\n", article.URL))
			result.WriteString("---\n\n")
		}
	}

	// Add metadata
	result.WriteString(fmt.Sprintf("**Search performed:** %s\n", time.Now().Format("2006-01-02 15:04:05")))

	if keyword != "" {
		result.WriteString(fmt.Sprintf("**Keyword:** %s\n", keyword))
	}

	result.WriteString("\n\n---\n\n")
	result.WriteString("PLEASE ALWAYS INCLUDE SOURCE URLs WHEN SUMMARIZING THE RESULTS \n")

	return result.String()
}
