// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
namespace Synctrain_App;

public sealed record FileAction(string Label, Func<Task> Run);
public sealed record FileRow(string Title, string Detail, Func<Task> Open, IReadOnlyList<FileAction> Actions)
{
    public string OpenLabel => "Open " + Title;
    public string ActionsLabel => "Actions for " + Title;
    public override string ToString() => Title;
}
