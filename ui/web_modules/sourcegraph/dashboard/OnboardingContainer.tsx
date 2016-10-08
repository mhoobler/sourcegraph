// tslint:disable: typedef ordered-imports

import * as React from "react";
import {Container} from "sourcegraph/Container";
import * as Dispatcher from "sourcegraph/Dispatcher";
import "sourcegraph/repo/RepoBackend"; // for side effects
import {RepoStore} from "sourcegraph/repo/RepoStore";
import * as RepoActions from "sourcegraph/repo/RepoActions";
import {ChromeExtensionOnboarding} from "sourcegraph/dashboard/ChromeExtensionOnboarding";
import {GitHubPrivateAuthOnboarding} from "sourcegraph/dashboard/GitHubPrivateAuthOnboarding";
import {Store} from "sourcegraph/Store";
import * as AnalyticsConstants from "sourcegraph/util/constants/AnalyticsConstants";
import {Dashboard} from "sourcegraph/dashboard/Dashboard";
import {context} from "sourcegraph/app/context";
import {EventLogger} from "sourcegraph/util/EventLogger";

interface Props {
	location?: any;
	currentStep?: string;
}

type State = any;

const reposQuerystring = "RemoteOnly=true";

export class OnboardingContainer extends Container<Props, State> {
	static contextTypes: React.ValidationMap<any> = {
		router: React.PropTypes.object.isRequired,
	};

	reconcileState(state: State, props: Props, context: any): void {
		Object.assign(state, props);
		state.repos = RepoStore.repos.list(reposQuerystring);
	}

	onStateTransition(prevState: State, nextState: State): void {
		if (nextState.repos !== prevState.repos) {
			Dispatcher.Backends.dispatch(new RepoActions.WantRepos(reposQuerystring));
		}
	}

	stores(): Store<any>[] {
		return [RepoStore];
	}

	_isPrivateCodeUser() {
		return context.gitHubToken && context.gitHubToken.scope && context.gitHubToken.scope.includes("repo") && context.gitHubToken.scope.includes("read:org");
	}

	_completeStep() {
		let nextStep = {};

		// TODO: update analytics to count only if chrome extension wasn't installed or Github not auth'd 
		if (this.props.currentStep === "chrome") {
			EventLogger.logEventForCategory(AnalyticsConstants.CATEGORY_ONBOARDING, AnalyticsConstants.ACTION_SUCCESS, "ChromeExtensionStepCompleted", {page_name: "ChromeExtensionOnboarding"});
			nextStep = {ob: "github"};
		} else if (this.props.currentStep === "github") {
			nextStep = {ob: "search"};
			EventLogger.logEventForCategory(AnalyticsConstants.CATEGORY_ONBOARDING, AnalyticsConstants.ACTION_SUCCESS, "GitHubStepCompleted", {page_name: "GitHubPrivateCodeOnboarding"});

			// This should be right after the Github onboarding step
			// Chrome extension will on receiving 401 from /rev endpoint
			// bring the user to /join?ob=github&rtg=${encodeURIComponent(window.location.href)}
			// Once the user has completed the github onboarding step (enabling private repos),
			// they should be taken back to Github if rtg was provided and is to a github URL
			const returnToGithub = this.props.location.query["rtg"] || null;
			if (returnToGithub) {
				const decodeUrl = decodeURIComponent(returnToGithub);
				const returnUrl = new URL(decodeUrl);

				if (returnUrl.origin.match(/https:\/\/(www\.)?github.com/)) {
					setTimeout(() => { window.location.href = decodeUrl; }, 500);
					return;
				}
			}
		}

		// Grab the current location and figure out where to go next.
		const newLoc = Object.assign({}, this.props.location, {query: nextStep});
		(this.context as any).router.replace(newLoc);
	}

	_renderOnboardingStep(): JSX.Element | null {
		if (this.props.currentStep === "chrome") {
			return <ChromeExtensionOnboarding completeStep={this._completeStep.bind(this)} location={this.props.location}/>;
		}

		if (this.props.currentStep === "github") {
			return <GitHubPrivateAuthOnboarding completeStep={this._completeStep.bind(this)} privateCodeAuthed={this._isPrivateCodeUser()} location={this.props.location}/>;
		}

		return <Dashboard location={this.props.location} completedBanner={true}/>;
	}

	render(): JSX.Element | null {
		return (<div>
			{this._renderOnboardingStep()}
		</div>);
	}
}
