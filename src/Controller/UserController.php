<?php
declare(strict_types=1);

namespace App\Controller;

use App\Entity\{Invite, PasswordResetToken};
use App\Repository\PasswordResetTokenRepository;
use App\Form\Data\{PasswordResetRequestData, PasswordResetData, RegisterData};
use App\Form\{PasswordResetRequestType, PasswordResetType, RegisterType};
use App\Repository\InviteRepository;
use App\User\{Exception\UserNotFoundException, InviteManager, PasswordResetManager, UserManager};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\Form\{Extension\Core\Type\SubmitType, FormError, FormInterface};
use Symfony\Component\HttpFoundation\{Request, Response};
use Symfony\Component\PasswordHasher\Hasher\PasswordHasherFactoryInterface;

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
        $data = new RegisterData($code);
        $form = $this->createRegisterForm($data, $code);

        /** @var Invite $invite */
        $invite = $inviteRepo->findOneBy(['code' => $code, 'usedBy' => null]);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            $user = $userManager->createUserByInvite(
                $data->username,
                $data->password,
                $data->email,
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
        $data = new PasswordResetRequestData();
        $form = $this->createResetRequestForm($data);

        $form->handleRequest($request);

        $message = null;

        if ($form->isSubmitted() && $form->isValid()) {
            try {
                $manager->sendResetLink($data->email);

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
        PasswordHasherFactoryInterface $hasherFactory,
        PasswordResetTokenRepository $tokenRepository,
    ): Response
    {
        $data = new PasswordResetData();
        $form = $this->createPasswordResetForm($data, $code);

        /** @var PasswordResetToken $token */
        $token = $tokenRepository->find($code);

        $form->handleRequest($request);

        if ($form->isSubmitted() && $form->isValid()) {
            if ($token && $token->isValid()) {
                $user = $token->getUser();
                $hasher = $hasherFactory->getPasswordHasher($user);
                $user->changePassword($hasher, $data->password);

                $em->remove($token);
                $em->flush();

                return $this->redirectToRoute('user_auth_login');
            }

            $form->addError(new FormError('Invalid token.'));
        }

        return $this->render('User/reset.html.twig', [
            'form' => $form->createView(),
        ]);
    }

    private function createResetRequestForm(PasswordResetRequestData $formData): FormInterface
    {
        return $this
            ->createForm(PasswordResetRequestType::class, $formData, [
                'action' => $this->generateUrl('user_reset_request'),
            ])
            ->add('submit', SubmitType::class);
    }

    private function createPasswordResetForm(PasswordResetData $data, string $code): FormInterface
    {
        return $this
            ->createForm(PasswordResetType::class, $data, [
                'action' => $this->generateUrl('user_reset', ['code' => $code]),
            ])
            ->add('submit', SubmitType::class);
    }

    private function createRegisterForm(RegisterData $formData, string $code): FormInterface
    {
        return $this
            ->createForm(RegisterType::class, $formData, [
                'action' => $this->generateUrl('user_register', ['code' => $code]),
            ])
            ->add('submit', SubmitType::class);
    }
}