<?php

namespace App\Controller;

use App\Form\LoginType;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\Form\Extension\Core\Type\SubmitType;
use Symfony\Component\Form\{FormError, FormInterface};
use Symfony\Component\HttpFoundation\{Request, Response};
use Symfony\Component\Security\Http\Authentication\AuthenticationUtils;
use Symfony\Component\Translation\TranslatorInterface;

class SecurityController extends Controller
{
    public function login(Request $request, AuthenticationUtils $authenticationUtils, TranslatorInterface $translator): Response
    {
        $lastError = $authenticationUtils->getLastAuthenticationError() ? $authenticationUtils->getLastAuthenticationError()->getMessage() : '';
        $lastUsername = $authenticationUtils->getLastUsername();

        $form = $this->createLoginForm($lastUsername);
        $form->handleRequest($request);

        if ($lastError) {
            $form->addError(new FormError($lastError));
        }

        return $this->render('Security/login.html.twig', ['form' => $form->createView()]);
    }

    private function createLoginForm(string $username): FormInterface
    {
        $form = $this->createForm(LoginType::class, null, [
            'action' => $this->generateUrl('user_login'),
        ]);
        $form->get('_username')->setData($username);
        $form->add('submit', SubmitType::class);

        return $form;
    }
}