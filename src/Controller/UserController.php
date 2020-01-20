<?php

namespace App\Controller;

use App\Entity\{Invite, PasswordResetToken};
use App\Repository\PasswordResetTokenRepository;
use App\Form\{Data\PasswordResetRequestData, Data\PasswordResetData, PasswordResetRequestType, PasswordResetType, RegisterType, Data\RegisterData};
use App\Repository\InviteRepository;
use App\User\{Exception\UserNotFoundException, InviteManager, PasswordResetManager, UserManager};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\Form\{Extension\Core\Type\SubmitType, FormError, FormInterface};
use Symfony\Component\HttpFoundation\{Request, Response};

class UserController extends AbstractController
{
    public function register(
        string $code,
        Request $request,
        EntityManagerInterface $em,
        UserManager $userManager,
        InviteManager $inviteManager,
        InviteRepository $inviteRepo
    ): Response {
        $formData = new RegisterData($code);
        $form = $this->createRegisterForm($formData, $code);

        /** @var Invite $invite */
        $invite = $inviteRepo->findOneBy(['code' => $code, 'usedBy' => null]);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            $user = $userManager->createUserByInvite(
                $formData->username,
                $formData->password,
                $formData->email,
                $invite
            );

            $inviteManager->createInvitesForUser($user);

            $em->flush();

            return $this->redirectToRoute('index');
        }

        return $this->render('User/register.html.twig', [
            'form' => $form->createView(),
            'invite' => $invite,
        ]);
    }

    public function requestReset(Request $request, EntityManagerInterface $em, PasswordResetManager $manager): Response
    {
        $formData = new PasswordResetRequestData();
        $form = $this->createResetRequestForm($formData);

        $form->handleRequest($request);

        $message = null;

        if ($form->isSubmitted() && $form->isValid()) {
            try {
                $manager->sendResetLink($formData->email);

                $message = 'Password reset link was sent';
            } catch (UserNotFoundException $e) {
                $form->addError(new FormError('User not found'));
            } catch (\Throwable $e) {
                \Sentry\captureException($e);
                $form->addError(new FormError('Something happened. Try again later or contact the administrator.'));
            }
        }

        return $this->render('User/reset.html.twig', [
            'form' => $form->createView(),
            'message' => $message,
        ]);
    }

    public function reset(
        string $code,
        Request $request,
        EntityManagerInterface $em,
        UserManager $manager,
        PasswordResetTokenRepository $tokenRepository
    ): Response
    {
        $formData = new PasswordResetData();
        $form = $this->createResetForm($formData, $code);

        /** @var PasswordResetToken $token */
        $token = $tokenRepository->find($code);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            if ($token && $token->isValid()) {
                $manager->changePassword($token->getUser(), $formData->password);

                $em->remove($token);
                $em->flush();

                return $this->redirectToRoute('user_auth_login');
            } else {
                $form->addError(new FormError('Invalid token.'));
            }
        }

        return $this->render('User/reset.html.twig', [
            'form' => $form->createView(),
        ]);
    }

    private function createResetRequestForm(PasswordResetRequestData $formData): FormInterface
    {
        $form = $this->createForm(PasswordResetRequestType::class, $formData, [
            'action' => $this->generateUrl('user_reset_request'),
        ]);
        $form->add('submit', SubmitType::class);

        return $form;
    }

    private function createResetForm(PasswordResetData $formData, string $code): FormInterface
    {
        $form = $this->createForm(PasswordResetType::class, $formData, [
            'action' => $this->generateUrl('user_reset', ['code' => $code]),
        ]);
        $form->add('submit', SubmitType::class);

        return $form;
    }

    private function createRegisterForm(RegisterData $formData, string $code): FormInterface
    {
        $form = $this->createForm(RegisterType::class, $formData, [
            'action' => $this->generateUrl('user_register', ['code' => $code]),
        ]);

        $form->add('submit', SubmitType::class);

        return $form;
    }
}