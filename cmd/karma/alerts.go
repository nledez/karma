package main

import (
	"fmt"
	"math"
	"sort"

	"github.com/gin-gonic/gin"
	"vbom.ml/util/sortorder"

	"github.com/prymitive/karma/internal/alertmanager"
	"github.com/prymitive/karma/internal/config"
	"github.com/prymitive/karma/internal/filters"
	"github.com/prymitive/karma/internal/models"
	"github.com/prymitive/karma/internal/slices"
	"github.com/prymitive/karma/internal/uri"

	log "github.com/sirupsen/logrus"
)

func getFiltersFromQuery(filterStrings []string) ([]filters.FilterT, bool) {
	validFilters := false
	matchFilters := []filters.FilterT{}
	for _, filterExpression := range filterStrings {
		f := filters.NewFilter(filterExpression)
		if f.GetIsValid() {
			validFilters = true
		}
		matchFilters = append(matchFilters, f)
	}
	return matchFilters, validFilters
}

func countLabel(countStore map[string]map[string]int, key string, val string) {
	if _, found := countStore[key]; !found {
		countStore[key] = make(map[string]int)
	}
	if _, found := countStore[key][val]; found {
		countStore[key][val]++
	} else {
		countStore[key][val] = 1
	}
}

func countersToLabelStats(counters map[string]map[string]int) models.LabelNameStatsList {
	data := models.LabelNameStatsList{}

	for name, valueMap := range counters {
		nameStats := models.LabelNameStats{
			Name:   name,
			Values: models.LabelValueStatsList{},
		}

		for value, hits := range valueMap {
			nameStats.Hits += hits
			valueStats := models.LabelValueStats{
				Value: value,
				Raw:   fmt.Sprintf("%s=%s", name, value),
				Hits:  hits,
			}
			nameStats.Values = append(nameStats.Values, valueStats)
		}

		// now that we have total hits we can calculate %
		var totalPercent int
		for i, value := range nameStats.Values {
			nameStats.Values[i].Percent = int(math.Floor((float64(value.Hits) / float64(nameStats.Hits)) * 100.0))
			totalPercent += nameStats.Values[i].Percent
		}
		sort.Sort(nameStats.Values)
		for totalPercent < 100 {
			for i := range nameStats.Values {
				nameStats.Values[i].Percent++
				totalPercent++
				if totalPercent >= 100 {
					break
				}
			}
		}

		// now that we have all % and values are sorted we can calculate offsets
		offset := 0
		for i, value := range nameStats.Values {
			nameStats.Values[i].Offset = offset
			offset += value.Percent
		}
		data = append(data, nameStats)
	}

	sort.Sort(data)

	return data
}

func getUpstreams() models.AlertmanagerAPISummary {
	summary := models.AlertmanagerAPISummary{}

	clusters := map[string][]string{}
	upstreams := alertmanager.GetAlertmanagers()
	for _, upstream := range upstreams {
		members := upstream.ClusterMemberNames()
		key, err := slices.StringSliceToSHA1(members)
		if err != nil {
			log.Errorf("slices.StringSliceToSHA1 error: %s", err)
			continue
		}
		if _, found := clusters[key]; !found {
			clusters[key] = members
		}

		u := models.AlertmanagerAPIStatus{
			Name:           upstream.Name,
			URI:            upstream.SanitizedURI(),
			PublicURI:      upstream.PublicURI(),
			Headers:        map[string]string{},
			Error:          upstream.Error(),
			Version:        upstream.Version(),
			Cluster:        upstream.ClusterID(),
			ClusterMembers: members,
		}
		if !upstream.ProxyRequests {
			for k, v := range uri.HeadersForBasicAuth(u.PublicURI) {
				u.Headers[k] = v
			}
			for k, v := range upstream.HTTPHeaders {
				u.Headers[k] = v
			}
		}
		summary.Instances = append(summary.Instances, u)

		summary.Counters.Total++
		if u.Error == "" {
			summary.Counters.Healthy++
		} else {
			summary.Counters.Failed++
		}
	}
	summary.Clusters = clusters

	return summary
}

func resolveLabelValue(name, value string) string {
	valueReplacements, found := config.Config.Grid.Sorting.CustomValues.Labels[name]
	if found {
		if replacement, ok := valueReplacements[value]; ok {
			return replacement
		}
	}
	return value
}

func getGroupLabel(group *models.APIAlertGroup, label string) string {
	if v, found := group.Labels[label]; found {
		return resolveLabelValue(label, v)
	}
	if v, found := group.Shared.Labels[label]; found {
		return resolveLabelValue(label, v)
	}
	if v, found := group.Alerts[0].Labels[label]; found {
		return resolveLabelValue(label, v)
	}
	return ""
}

func sortByStartsAt(i, j int, groups []models.APIAlertGroup, sortReverse bool) bool {
	if sortReverse {
		return groups[i].LatestStartsAt.After(groups[j].LatestStartsAt)
	}
	return groups[i].LatestStartsAt.Before(groups[j].LatestStartsAt)
}

func sortAlertGroups(c *gin.Context, groupsMap map[string]models.APIAlertGroup) []models.APIAlertGroup {
	groups := make([]models.APIAlertGroup, 0, len(groupsMap))

	sortOrder, found := c.GetQuery("sortOrder")
	if !found || sortOrder == "" {
		sortOrder = config.Config.Grid.Sorting.Order
	}

	sortReverse, found := c.GetQuery("sortReverse")
	if !found || (sortReverse != "0" && sortReverse != "1") {
		if config.Config.Grid.Sorting.Reverse {
			sortReverse = "1"
		} else {
			sortReverse = "0"
		}
	}

	sortLabel, found := c.GetQuery("sortLabel")
	if !found || sortLabel == "" {
		sortLabel = config.Config.Grid.Sorting.Label
	}

	for _, g := range groupsMap {
		groups = append(groups, g)
	}

	switch sortOrder {
	case "startsAt":
		sort.Slice(groups, func(i, j int) bool {
			return sortByStartsAt(i, j, groups, sortReverse == "1")
		})
	case "label":
		sort.Slice(groups, func(i, j int) bool {
			vi := getGroupLabel(&groups[i], sortLabel)
			vj := getGroupLabel(&groups[j], sortLabel)
			if vi == "" && vj == "" {
				// both groups lack this label, fallback to timestamp sort
				return sortByStartsAt(i, j, groups, true)
			}

			if vi == "" {
				// first label is missing
				return sortReverse != "0"
			}
			if vj == "" {
				// second label is missing
				return sortReverse == "0"
			}
			if vi == vj {
				// both labels are equal fallback to timestamp sort
				return sortByStartsAt(i, j, groups, true)
			}
			// finnally return groups sorted by label
			if sortReverse == "1" {
				return !sortorder.NaturalLess(vi, vj)
			}
			return sortorder.NaturalLess(vi, vj)
		})
	default:
		// sort alert groups so they are always returned in the same order
		// use group ID which is unique and immutable
		sort.Slice(groups, func(i, j int) bool {
			if sortReverse == "1" {
				return groups[i].ID < groups[j].ID
			}
			return groups[i].ID > groups[j].ID
		})
	}

	return groups
}
