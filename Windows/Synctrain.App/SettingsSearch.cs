// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
using Microsoft.UI.Xaml;
using Microsoft.UI.Xaml.Controls;
using Microsoft.UI.Xaml.Automation;
using System.Globalization;
using System.Text;
using System.Text.Json.Nodes;

namespace Synctrain_App;

public sealed partial class MainPage
{
    private sealed record SettingsResult(string Title, string Breadcrumb, JsonObject? Entry, int Score = 0);
    private JsonArray settingsIndex = [];
    private string settingsQuery = "", settingsIndexMode = "", settingsIndexRevision = "", settingsIndexError = "";
    private bool settingsIndexLoading, settingsResultsPage, showingSettingsResults;

    private static string NormalizeSettingsSearch(string value)
    {
        var text = new StringBuilder();
        foreach (var c in value.Normalize(NormalizationForm.FormD))
            if (CharUnicodeInfo.GetUnicodeCategory(c) != UnicodeCategory.NonSpacingMark) text.Append(char.ToLowerInvariant(c));
        return text.ToString();
    }

    private List<SettingsResult> SettingsMatches(string query)
    {
        var entries = new List<JsonObject>();
        foreach (var category in catalog["settings"]!.AsArray())
            foreach (var section in category!["sections"]!.AsArray())
            {
                var entry = section!.DeepClone().AsObject();
                entry["id"] = S(category,"id") + "." + S(section,"id");
                entry["category"] = S(category,"id"); entry["breadcrumb"] = S(category,"title");
                entries.Add(entry);
            }
        foreach (var entry in catalog["search"]!.AsArray())
            if (S(entry,"section").Length == 0 && (entry?["platforms"] is not JsonArray platforms || platforms.Any(item => item?.ToString() == "windows"))) entries.Add(entry!.AsObject());
        entries.AddRange(settingsIndex.OfType<JsonObject>());
        var normalized = NormalizeSettingsSearch(query).Trim();
        if (normalized.Length == 0) return [];
        var words = normalized.Split((char[]?)null, StringSplitOptions.RemoveEmptyEntries);
        return entries.Select(entry => {
            var title = S(entry,"title"); var label = NormalizeSettingsSearch(title);
            var corpus = NormalizeSettingsSearch(title + " " + S(entry,"breadcrumb") + " " + S(entry,"field") + " " + string.Join(" ",entry["keywords"]?.AsArray().Select(value => value?.ToString()) ?? []));
            if (!words.All(corpus.Contains)) return null;
            var score = label == normalized ? 0 : label.StartsWith(normalized,StringComparison.Ordinal) ? 1 : label.Contains(normalized,StringComparison.Ordinal) ? 2 : 3;
            return new SettingsResult(title,S(entry,"breadcrumb"),entry,score);
        }).OfType<SettingsResult>().OrderBy(result => result.Score).ThenBy(result => S(result.Entry,"id"),StringComparer.Ordinal).ToList();
    }

    private async Task LoadSettingsIndex(bool force = false)
    {
        var mode = S(status,"mode"); var revision = S(status,"revision");
        if (mode != settingsIndexMode) { settingsIndex = []; settingsIndexRevision = ""; settingsIndexMode = mode; settingsIndexError=""; UpdateSettingsSuggestions(); }
        if (!page.StartsWith("settings:",StringComparison.Ordinal) || mode != "synchronize" || settingsIndexLoading || !force && settingsIndexRevision == revision && settingsIndexRevision.Length > 0) return;
        settingsIndexLoading = true;
        var previousIndex = settingsIndex.ToJsonString(); var previousError = settingsIndexError;
        try
        {
            var index = await host.Call("desktop.settings.index",timeoutSeconds:15);
            if (S(status,"mode") != mode || S(status,"revision") != revision || !page.StartsWith("settings:",StringComparison.Ordinal)) return;
            settingsIndex = index?.AsArray() ?? []; settingsIndexRevision = revision; settingsIndexError = "";
        }
        catch (Exception error) { if (S(status,"mode") == mode) settingsIndexError = error.Message; }
        finally { settingsIndexLoading = false; }
        if (previousIndex != settingsIndex.ToJsonString() || previousError != settingsIndexError) UpdateSettingsSuggestions();
    }

    private void UpdateSettingsSuggestions()
    {
        var matches = SettingsMatches(settingsQuery);
        if (matches.Count == 0 && settingsQuery.Trim().Length > 0) matches.Add(new("No Results",settingsIndexError.Length > 0 ? settingsIndexError : "Try another word or phrase.",null));
        else if (settingsIndexError.Length > 0) matches.Insert(0,new("Some settings could not be loaded",settingsIndexError,null));
        SettingsSearchBox.ItemsSource = matches;
        if (showingSettingsResults && settingsQuery.Trim().Length > 0) ShowSettingsResults();
    }

    private void FocusSettingsSearch(Microsoft.UI.Xaml.Input.KeyboardAccelerator sender, Microsoft.UI.Xaml.Input.KeyboardAcceleratorInvokedEventArgs args)
    {
        if (!page.StartsWith("settings:",StringComparison.Ordinal)) return;
        Navigation.IsPaneOpen=true; SettingsSearchBox.Focus(FocusState.Programmatic); args.Handled=true;
    }

    private async void SettingsSearchChanged(AutoSuggestBox sender, AutoSuggestBoxTextChangedEventArgs args)
    {
        if (args.Reason != AutoSuggestionBoxTextChangeReason.UserInput) return;
        settingsQuery = sender.Text;
        UpdateSettingsSuggestions();
        if (settingsQuery.Trim().Length == 0)
        {
            settingsResultsPage = false;
            // Clearing a query only replaces the results body. An open editor
            // keeps its draft and returns to the category when Back is chosen.
            if (showingSettingsResults && !loadingEditor) await Run(Refresh);
        }
    }

    private async void SettingsQuerySubmitted(AutoSuggestBox sender, AutoSuggestBoxQuerySubmittedEventArgs args)
    {
        await Run(async () => {
            sender.Text = settingsQuery;
            if (args.ChosenSuggestion is SettingsResult { Entry: not null } result) await OpenSettingsResult(result.Entry);
            else if (args.ChosenSuggestion is null && settingsQuery.Trim().Length > 0) {
                if (loadingEditor || !await MayLeaveEditor()) return;
                await CloseEditor(); settingsResultsPage = true; detailRoute = null; detailParents.Clear();
                SetRenderGeneration(++generation);
                ShowSettingsResults();
            }
        });
    }

    private void ShowSettingsResults()
    {
        ShowBody(); showingSettingsResults = true; Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = "Search Results";
        Command("Back",Symbol.Back,async () => { settingsResultsPage = false; await Refresh(); });
        if (settingsIndexError.Length > 0) Text("Some settings could not be loaded: " + settingsIndexError);
        var matches = SettingsMatches(settingsQuery);
        if (matches.Count == 0) Text("No Results");
        foreach (var result in matches) Row(result.Title,result.Breadcrumb,ActionButton("Open",() => OpenSettingsResult(result.Entry!)));
    }

    private async Task OpenSettingsResult(JsonObject entry)
    {
        if (loadingEditor || !await MayLeaveEditor()) return;
        await CloseEditor(); detailRoute = null; detailParents.Clear();
        SetRenderGeneration(++generation);
        page="settings:"+S(entry,"category"); BuildNavigation();
        var capability = S(entry,"capability"); var mode = S(status,"mode");
        if (capability != "always" && capability != mode) {
            ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = S(entry,"title");
            Text("Available in " + capability + " mode. Change the operating mode in General.");
            Command("Back",Symbol.Back,Refresh); return;
        }
        if (S(entry,"section").Length > 0 || S(entry,"action") == "proxy") { await EditSettings(S(entry,"action"),S(entry,"anchor"),S(entry,"field")); return; }
        ShowBody(); Body.Children.Clear(); Commands.PrimaryCommands.Clear(); Heading.Text = S(entry,"title");
        Command("Back",Symbol.Back,Refresh);
        if (S(entry,"action") == "remoteService") { await Gateway(S(entry,"anchor")); return; }
        else if (S(entry,"action") == "gateways") await Gateways();
        else if (S(entry,"action") == "historySources") await HistorySources();
        else await CatalogSetting(S(entry,"action"),S(entry,"title"),mode);
        if (Body.Children.FirstOrDefault() is FrameworkElement control) { control.StartBringIntoView(); if (control is Control focus) focus.Focus(FocusState.Programmatic); }
    }
}
